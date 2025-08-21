package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Set upload limit
	uploadLimit := int64(1024 * 1024 * 1024) // 1GB
	http.MaxBytesReader(w, r.Body, uploadLimit)

	// Get video ID from path
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// Authenticate user
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Invalid token", err)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// Check if user owns video
	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}
	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't own this video", err)
		return
	}

	// Parse uploaded multipart file
	video, videoHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video", err)
		return
	}
	defer video.Close()

	// Ensure the file is an MP4 video
	fileType, _, err := mime.ParseMediaType(videoHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error while parsing file type", err)
		return
	}
	if fileType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", err)
		return
	}

	// Create temp file
	videoFile, err := os.CreateTemp("", "tubely-video-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(videoFile.Name())
	defer videoFile.Close()

	// Write data to temp file
	_, err = io.Copy(videoFile, video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write to temp file", err)
		return
	}

	// Reset temp file pointer, so we can read it from beginnig
	_, err = videoFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	// Put the file to s3
	randBytes := make([]byte, 32)
	rand.Read(randBytes)
	s3Key := fmt.Sprintf("%v.mp4", base64.RawURLEncoding.EncodeToString(randBytes))
	putObjectInput := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3Key,
		Body:        videoFile,
		ContentType: aws.String("video/mp4"),
	}
	log.Printf("Uploading video to S3")
	_, err = cfg.s3Client.PutObject(r.Context(), putObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}
	log.Printf("Video uploaded to S3")

	videoUrl := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, s3Key)
	dbVideo.VideoURL = &videoUrl

	// Update the video in the database
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video in database", err)
		return
	}

	// Return the updated video to the client
	respondWithJSON(w, http.StatusOK, dbVideo)
}
