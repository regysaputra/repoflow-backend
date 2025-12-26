package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
)

const port = ":8081"

type FileHandler struct {
	Client *s3.Client
	Bucket string
}

type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func main() {
	// Load config from env
	if _, err := os.Stat(".env"); err == nil {
		godotenv.Load()
		log.Println("Loaded .env file for local development")
	}

	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKey := os.Getenv("R2_ACCESS_KEY")
	secretKey := os.Getenv("R2_SECRET_KEY")
	bucketName := os.Getenv("R2_BUCKET")

	if accountID == "" || accessKey == "" || secretKey == "" || bucketName == "" {
		log.Fatal("Error: R2_ACCOUNT_ID, R2_ACCESS_KEY, R2_SECRET_KEY, and R2_BUCKET are required")
	}

	// 1. Load the Default Config (No resolver needed here anymore)
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		log.Fatal(err)
	}

	// 2. Create the Client with BaseEndpoint
	// This replaces the deprecated EndpointResolverWithOptions
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID))
	})

	handler := &FileHandler{
		Client: client,
		Bucket: bucketName,
	}

	router := chi.NewRouter()
	router.Use(middleware.Recoverer)

	router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		SendJSON(w, http.StatusOK, Response{true, "OK"})
	})

	router.Group(func(r chi.Router) {
		r.Use(AuthMiddleware)
		r.Post("/push", handler.pushHandler)
		r.Get("/pull", handler.pullHandler)
		r.Get("/list", handler.listHandler)
		r.Post("/push-dir", handler.pushDirHandler)
		r.Get("/pull-dir", handler.pullDirHandler)
	})

	log.Fatal(http.ListenAndServe(port, router))
}

func (h *FileHandler) pushHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user ID from context
	userID := r.Context().Value(userIDKey).(string)

	// Parse max 100MB
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		SendJSON(w, http.StatusBadRequest, Response{false, "Failed to parse form"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		SendJSON(w, http.StatusBadRequest, Response{false, "No file provided"})
		return
	}
	defer file.Close()

	key := fmt.Sprintf("%s/%s", userID, header.Filename)

	// Upload to R2
	_, err = h.Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket: aws.String(h.Bucket),
		Key:    aws.String(key),
		Body:   file,
		// ContentLength is helpful for S3 to know size upfront
		ContentLength: aws.Int64(header.Size),
		Metadata: map[string]string{
			"owner-id": userID,
		},
	})

	if err != nil {
		log.Printf("R2 Upload Error: %v", err)
		SendJSON(w, http.StatusInternalServerError, Response{false, "Failed to upload to R2"})
		return
	}

	log.Printf("File uploaded to R2: %s", key)
	SendJSON(w, http.StatusOK, Response{true, fmt.Sprintf("File '%s' uploaded successfully", key)})
}

func (h *FileHandler) pullHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user ID from context
	userID := r.Context().Value(userIDKey).(string)

	filename := r.URL.Query().Get("file")
	if filename == "" {
		SendJSON(w, http.StatusBadRequest, Response{false, "File parameter required"})
		return
	}

	key := fmt.Sprintf("%s/%s", userID, filename)

	// Request object from R2
	output, err := h.Client.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(h.Bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		// Differentiate between "Not Found" and other errors if needed
		log.Printf("Download error: %v", err)
		SendJSON(w, http.StatusNotFound, Response{false, "File not found or access denied"})
		return
	}
	defer output.Body.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(filename)))
	w.Header().Set("Content-Type", "application/octet-stream")
	if output.ContentLength != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", *output.ContentLength))
	}

	// Stream R2 -> User
	if _, err := io.Copy(w, output.Body); err != nil {
		log.Printf("Stream error: %v", err)
	}
}

func (h *FileHandler) listHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user ID from context
	userID := r.Context().Value(userIDKey).(string)
	userPrefix := userID + "/"

	// List objects in R2
	output, err := h.Client.ListObjectsV2(r.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String(h.Bucket),
		Prefix: aws.String(userPrefix),
	})

	if err != nil {
		log.Printf("List error: %v", err)
		SendJSON(w, http.StatusInternalServerError, Response{false, "Failed to list files"})
		return
	}

	var fileList []string
	for _, obj := range output.Contents {
		cleanName := strings.TrimPrefix(*obj.Key, userPrefix)

		if cleanName == "" {
			continue
		}

		fileList = append(fileList, cleanName)
	}

	SendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"files":   fileList,
	})
}

func (h *FileHandler) pushDirHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user ID from context
	userID := r.Context().Value(userIDKey).(string)

	// Parse max 500MB
	if err := r.ParseMultipartForm(500 << 20); err != nil {
		SendJSON(w, http.StatusBadRequest, Response{false, "Failed to parse form"})
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		SendJSON(w, http.StatusBadRequest, Response{false, "No file provided"})
		return
	}
	defer file.Close()

	// Determine the base prefix
	basePrefix := userID + "/"

	// Optional prefix from query param
	subDir := r.URL.Query().Get("name")
	if subDir != "" {
		// Clean the input to prevent ".." attacks, then ensure trailing slash
		cleanSub := filepath.Clean(subDir)
		if cleanSub == "." || cleanSub == "/" {
			cleanSub = ""
		} else {
			// Ensure we don't start with a slash (to avoid double //)
			cleanSub = strings.TrimPrefix(cleanSub, "/")
			// Ensure we end with a slash
			if !strings.HasSuffix(cleanSub, "/") {
				cleanSub += "/"
			}
			basePrefix += cleanSub
		}
	}

	gzr, err := gzip.NewReader(file)
	if err != nil {
		SendJSON(w, http.StatusBadRequest, Response{false, "Invalid gzip file"})
		return
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	fileCount := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			SendJSON(w, http.StatusBadRequest, Response{false, "Invalid tar file"})
			return
		}

		// Skip directories (R2 creates "folders" automatically based on keys)
		if header.Typeflag == tar.TypeDir {
			continue
		}

		// Construct Key (Prefix + Filename)
		// Clean path to ensure no ".." exploits, though R2 treats keys as literal strings anyway
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "/") {
			cleanName = cleanName[1:] // Remove leading slash
		}
		objectKey := basePrefix + cleanName

		// Upload individual file from the tar stream directly to R2
		// Note: header.Size is crucial here so S3 client doesn't need to buffer the stream
		_, err = h.Client.PutObject(r.Context(), &s3.PutObjectInput{
			Bucket:        aws.String(h.Bucket),
			Key:           aws.String(objectKey),
			Body:          tr, // tar reader acts as an io.Reader for the current file
			ContentLength: aws.Int64(header.Size),
			Metadata: map[string]string{
				"owner-id": userID,
			},
		})

		if err != nil {
			log.Printf("Failed to upload part of dir: %s. Error: %v", objectKey, err)
			continue // Strategy: Log and continue, or fail hard?
		}

		fileCount++
	}

	log.Printf("Directory upload complete. Processed %d files.", fileCount)
	SendJSON(w, http.StatusOK, Response{true, fmt.Sprintf("Extracted and uploaded %d files to %s", fileCount, h.Bucket)})
}

func (h *FileHandler) pullDirHandler(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	dirName := r.URL.Query().Get("dir")
	if dirName == "" {
		SendJSON(w, http.StatusBadRequest, Response{false, "Directory parameter required"})
		return
	}

	// Prepare prefix
	// Clean path and ensure it ends with / to match folder structure
	cleanDir := filepath.Clean(dirName)
	if cleanDir == "." || cleanDir == "/" {
		cleanDir = ""
	} else {
		cleanDir = strings.Trim(cleanDir, "/") + "/"
	}

	// Full prefix: user_123/my_backup/
	prefix := userID + "/" + cleanDir

	// 2. Set Response Headers for Download
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.tar.gz\"", strings.Trim(cleanDir, "/")))
	w.Header().Set("Content-Type", "application/x-gzip")

	// 3. Setup Compression Streams
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	// 4. List Objects (Handle Pagination for large folders)
	paginator := s3.NewListObjectsV2Paginator(h.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(h.Bucket),
		Prefix: aws.String(prefix),
	})

	fileCount := 0

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(r.Context())
		if err != nil {
			log.Printf("Failed to list objects: %v", err)
			return // Cannot write JSON error because headers are already sent
		}

		for _, obj := range page.Contents {
			// Skip if it's the folder itself (0 byte object ending in /)
			if strings.HasSuffix(*obj.Key, "/") {
				continue
			}

			// 5. Download File from R2
			fileObj, err := h.Client.GetObject(r.Context(), &s3.GetObjectInput{
				Bucket: aws.String(h.Bucket),
				Key:    obj.Key,
			})
			if err != nil {
				log.Printf("Failed to download %s: %v", *obj.Key, err)
				continue
			}

			// 6. Create Tar Header
			// We want the path inside the tar to be relative.
			// If R2 key is "user_123/photos/summer/img.jpg" and we requested "photos",
			// We want the tar entry to be "photos/summer/img.jpg" or "summer/img.jpg".
			// Let's strip the userID prefix to keep it clean.
			relPath := strings.TrimPrefix(*obj.Key, userID+"/")

			header := &tar.Header{
				Name: relPath,
				Size: *obj.Size,
				Mode: 0644,
			}

			if err := tw.WriteHeader(header); err != nil {
				log.Printf("Failed to write header for %s: %v", relPath, err)
				fileObj.Body.Close()
				continue
			}

			// 7. Stream content R2 -> Tar
			if _, err := io.Copy(tw, fileObj.Body); err != nil {
				log.Printf("Failed to copy body for %s: %v", relPath, err)
			}
			fileObj.Body.Close()
			fileCount++
		}
	}

	log.Printf("Downloaded directory '%s' (%d files) for user %s", dirName, fileCount, userID)
}

func SendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
