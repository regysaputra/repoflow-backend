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

func SendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
