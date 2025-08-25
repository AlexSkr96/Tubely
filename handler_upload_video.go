package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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
	videoFile, err := os.CreateTemp("", "tubely-video-s3.mp4")
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

	aspectRatioPrefix, err := getVideoAspectRationPrefix(videoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio prefix", err)
		return
	}

	// Reset temp file pointer, so we can read it from beginnig
	_, err = videoFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	// Process video to move metadata to front for more efficient streaming
	optimizedVideoPath, err := processVideoForFastStart(videoFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	optimizedVideoFile, err := os.Open(optimizedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening optimized video", err)
		return
	}

	// Upload the file to s3
	randBytes := make([]byte, 32)
	rand.Read(randBytes)
	s3Key := fmt.Sprintf("%v/%v.mp4", aspectRatioPrefix, base64.RawURLEncoding.EncodeToString(randBytes))
	putObjectInput := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3Key,
		Body:        optimizedVideoFile,
		ContentType: aws.String("video/mp4"),
	}
	log.Printf("Uploading video to S3")
	_, err = cfg.s3Client.PutObject(r.Context(), putObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}
	log.Printf("Video uploaded to S3")

	// videoUrl := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, s3Key)
	// videoUrl := fmt.Sprintf("%v,%v", cfg.s3Bucket, s3Key)
	videoUrl := fmt.Sprintf("https://%v/%v", cfg.s3CfDistribution, s3Key)
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

func getVideoAspectRationPrefix(filePath string) (string, error) {
	type stream struct {
		DisplayAspectRatio string `json:"display_aspect_ratio"`
		Width              int    `json:"width"`
		Height             int    `json:"height"`
		CodecType          string `json:"codec_type"`
	}

	type ffprobeOut struct {
		Streams []stream `json:"streams"`
	}

	cmd := exec.Command(
		"ffprobe",
		"-v",
		"error",
		"-print_format",
		"json",
		"-show_streams",
		filePath,
	)
	var cmdOutput bytes.Buffer
	cmd.Stdout = &cmdOutput

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error while running ffprobe: %s", err)
	}

	var fOut ffprobeOut
	decoder := json.NewDecoder(&cmdOutput)
	err = decoder.Decode(&fOut)
	if err != nil {
		return "", fmt.Errorf("error while decoding ffprobe output: %s", err)
	}

	// Find the first video stream
	for _, stream := range fOut.Streams {
		if stream.CodecType == "video" {
			switch stream.DisplayAspectRatio {
			case "16:9":
				return "landscape", nil
			case "9:16":
				return "portrait", nil
			default:
				return "other", nil
			}
		}
	}

	return "other", fmt.Errorf("no video stream found")
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error while running ffmpeg: %s", err)
	}

	return outputFilePath, nil
}
