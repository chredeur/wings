package router

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/middleware"
)

const (
	uploadBufferSize = 1024 * 1024
)

// UploadSession tracks the state of a chunked file upload.
type UploadSession struct {
	ID             string
	ServerID       string
	FilePath       string
	FileSize       int64
	ChunkSize      int64
	TotalChunks    int
	ReceivedChunks map[int]bool
	CreatedAt      time.Time
	mu             sync.RWMutex
}

// ChunkManager manages concurrent upload sessions across multiple servers.
type ChunkManager struct {
	sessions map[string]*UploadSession
	mu       sync.RWMutex
}

var globalChunkManager = &ChunkManager{
	sessions: make(map[string]*UploadSession),
}

func (cm *ChunkManager) CreateSession(serverID, filePath string, fileSize, chunkSize int64) (*UploadSession, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	activeSessions := 0
	for _, session := range cm.sessions {
		if session.ServerID == serverID {
			activeSessions++
		}
	}

	maxConcurrent := config.Get().ChunkedUpload.MaxConcurrentUploads
	if activeSessions >= maxConcurrent {
		return nil, errors.New(fmt.Sprintf("maximum concurrent uploads (%d) reached for this server", maxConcurrent))
	}

	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)

	session := &UploadSession{
		ID:             uuid.New().String(),
		ServerID:       serverID,
		FilePath:       filePath,
		FileSize:       fileSize,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		ReceivedChunks: make(map[int]bool),
		CreatedAt:      time.Now(),
	}

	cm.sessions[session.ID] = session
	return session, nil
}

func (cm *ChunkManager) GetSession(uploadID string) (*UploadSession, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	session, exists := cm.sessions[uploadID]
	return session, exists
}

func (cm *ChunkManager) DeleteSession(uploadID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.sessions, uploadID)
}

func (cm *ChunkManager) CleanupExpiredSessions() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	expirationHours := time.Duration(config.Get().ChunkedUpload.ChunkExpirationHours) * time.Hour
	now := time.Now()

	for id, session := range cm.sessions {
		if now.Sub(session.CreatedAt) > expirationHours {
			tmpDir := filepath.Join(os.TempDir(), "wings-uploads", id)
			os.RemoveAll(tmpDir)
			delete(cm.sessions, id)
		}
	}
}

func (s *UploadSession) MarkChunkReceived(chunkIndex int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReceivedChunks[chunkIndex] = true
}

func (s *UploadSession) IsChunkReceived(chunkIndex int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ReceivedChunks[chunkIndex]
}

func (s *UploadSession) AllChunksReceived() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ReceivedChunks) == s.TotalChunks
}

func (s *UploadSession) GetProgress() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.TotalChunks == 0 {
		return 0
	}
	return float64(len(s.ReceivedChunks)) / float64(s.TotalChunks) * 100
}

// postInitChunkedUpload initializes a new chunked upload session for large files.
// The client specifies the file path, total size, and preferred chunk size.
// Returns a unique upload ID and calculated total chunks needed.
func postInitChunkedUpload(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var data struct {
		Path      string `json:"path" binding:"required"`
		FileSize  int64  `json:"file_size" binding:"required"`
		ChunkSize int64  `json:"chunk_size" binding:"required"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	cleanPath := filepath.Clean(data.Path)
	if err := s.Filesystem().IsIgnored(cleanPath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	maxFileSize := config.Get().ChunkedUpload.MaxFileSize * 1024 * 1024
	if data.FileSize > maxFileSize {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("File size exceeds maximum allowed size of %d MB", config.Get().ChunkedUpload.MaxFileSize),
		})
		return
	}

	if err := s.Filesystem().HasSpaceErr(true); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	session, err := globalChunkManager.CreateSession(s.ID(), cleanPath, data.FileSize, data.ChunkSize)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": err.Error(),
		})
		return
	}

	tmpDir := filepath.Join(os.TempDir(), "wings-uploads", session.ID)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		globalChunkManager.DeleteSession(session.ID)
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":    session.ID,
		"total_chunks": session.TotalChunks,
		"chunk_size":   session.ChunkSize,
	})
}

// postUploadChunk receives a single chunk of data for an active upload session.
// Chunks can be uploaded in any order and duplicates are safely ignored.
// Uses buffered I/O for optimal performance on high-bandwidth networks.
func postUploadChunk(c *gin.Context) {
	s := middleware.ExtractServer(c)

	uploadID := c.GetHeader("X-Upload-Id")
	chunkIndexStr := c.GetHeader("X-Chunk-Index")
	totalChunksStr := c.GetHeader("X-Total-Chunks")

	if uploadID == "" || chunkIndexStr == "" || totalChunksStr == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Missing required headers: X-Upload-Id, X-Chunk-Index, X-Total-Chunks",
		})
		return
	}

	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Invalid chunk index",
		})
		return
	}

	totalChunks, err := strconv.Atoi(totalChunksStr)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Invalid total chunks",
		})
		return
	}

	session, exists := globalChunkManager.GetSession(uploadID)
	if !exists {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "Upload session not found",
		})
		return
	}

	if session.ServerID != s.ID() {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "Upload session does not belong to this server",
		})
		return
	}

	if totalChunks != session.TotalChunks {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Total chunks mismatch",
		})
		return
	}

	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "Invalid chunk index",
		})
		return
	}

	if session.IsChunkReceived(chunkIndex) {
		c.JSON(http.StatusOK, gin.H{
			"chunk_index":    chunkIndex,
			"status":         "already_received",
			"bytes_received": session.ChunkSize,
			"total_bytes":    session.FileSize,
			"progress":       session.GetProgress(),
		})
		return
	}

	tmpDir := filepath.Join(os.TempDir(), "wings-uploads", session.ID)
	chunkPath := filepath.Join(tmpDir, fmt.Sprintf("chunk_%d", chunkIndex))

	file, err := os.Create(chunkPath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer file.Close()

	bufferedWriter := bufio.NewWriterSize(file, uploadBufferSize)
	written, err := io.Copy(bufferedWriter, c.Request.Body)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	if err := bufferedWriter.Flush(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	session.MarkChunkReceived(chunkIndex)

	c.JSON(http.StatusOK, gin.H{
		"chunk_index":    chunkIndex,
		"status":         "received",
		"bytes_received": written,
		"total_bytes":    session.FileSize,
		"progress":       session.GetProgress(),
	})
}

// postFinalizeUpload assembles all received chunks into the final file.
// Verifies all chunks are present, reassembles them sequentially, computes
// a SHA256 checksum, and sets proper file ownership before cleanup.
func postFinalizeUpload(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var data struct {
		UploadID string `json:"upload_id" binding:"required"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	session, exists := globalChunkManager.GetSession(data.UploadID)
	if !exists {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "Upload session not found",
		})
		return
	}

	if session.ServerID != s.ID() {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "Upload session does not belong to this server",
		})
		return
	}

	if !session.AllChunksReceived() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":           "Not all chunks have been received",
			"received_chunks": len(session.ReceivedChunks),
			"total_chunks":    session.TotalChunks,
		})
		return
	}

	tmpDir := filepath.Join(os.TempDir(), "wings-uploads", session.ID)
	defer func() {
		os.RemoveAll(tmpDir)
		globalChunkManager.DeleteSession(session.ID)
	}()

	finalPath := filepath.Join(s.Filesystem().Path(), session.FilePath)
	finalDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	finalFile, err := os.Create(finalPath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer finalFile.Close()

	hash := sha256.New()
	var totalWritten int64

	for i := 0; i < session.TotalChunks; i++ {
		chunkPath := filepath.Join(tmpDir, fmt.Sprintf("chunk_%d", i))
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}

		written, err := io.Copy(io.MultiWriter(finalFile, hash), chunkFile)
		chunkFile.Close()
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		totalWritten += written
	}

	if err := s.Filesystem().Chown(session.FilePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	checksum := hex.EncodeToString(hash.Sum(nil))

	c.JSON(http.StatusOK, gin.H{
		"status":    "completed",
		"file_path": session.FilePath,
		"file_size": totalWritten,
		"checksum":  "sha256:" + checksum,
	})
}

// deleteChunkedUpload cancels an active upload session and removes all stored chunks.
func deleteChunkedUpload(c *gin.Context) {
	s := middleware.ExtractServer(c)
	uploadID := c.Param("upload_id")

	session, exists := globalChunkManager.GetSession(uploadID)
	if !exists {
		c.Status(http.StatusNoContent)
		return
	}

	if session.ServerID != s.ID() {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "Upload session does not belong to this server",
		})
		return
	}

	tmpDir := filepath.Join(os.TempDir(), "wings-uploads", session.ID)
	os.RemoveAll(tmpDir)
	globalChunkManager.DeleteSession(session.ID)

	c.Status(http.StatusNoContent)
}

func init() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			globalChunkManager.CleanupExpiredSessions()
		}
	}()
}
