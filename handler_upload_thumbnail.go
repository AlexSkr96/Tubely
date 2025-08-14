package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20 // 10MB

	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusUnprocessableEntity, "Couldn't parse form", err)
		return
	}
	multiPartFile, multiPartHeader, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusUnprocessableEntity, "Couldn't form file", err)
		return
	}
	contentType := multiPartHeader.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusUnprocessableEntity, "Couldn't parse media type", err)
		return
	}
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported media type", nil)
		return
	}

	reader := io.Reader(multiPartFile)
	bytes, err := io.ReadAll(reader)
	if err != nil {
		respondWithError(w, http.StatusUnprocessableEntity, "Couldn't read file", err)
		return
	}

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}
	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "", nil)
	}

	thumbnail := thumbnail{
		mediaType: contentType,
		data:      bytes,
	}
	videoThumbnails[videoID] = thumbnail

	randBytes := make([]byte, 32)
	rand.Read(randBytes)
	thumbnailFileName := base64.RawStdEncoding.EncodeToString(randBytes)
	thumbnailFileName = strings.ReplaceAll(thumbnailFileName, "/", "")

	fileExtension := strings.Split(mediaType, "/")[1]
	filePath := filepath.Join(cfg.assetsRoot, thumbnailFileName+"."+fileExtension)
	err = os.WriteFile(filePath, bytes, os.ModePerm)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Couldn't write file at %v", filePath), err)
		return
	}
	thumbnailURL := "http://localhost:" + cfg.port + "/assets/" + thumbnailFileName + "." + fileExtension

	dbVideo.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}
