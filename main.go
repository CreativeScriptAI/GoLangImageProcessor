package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/joho/godotenv"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/middleware/stdlib"
	"github.com/ulule/limiter/v3/drivers/store/memory"
)

// Optimized configurable settings
const (
	MaxUploadSize      = 50 << 20 // 50MB total upload
	DefaultQuality     = 80
	MaxWidth           = 2500             // Maximum width before resizing
	RateLimit          = "100-M"          // 100 requests per minute
	ServerTimeout      = 60 * time.Second // Increased for large files
	Port               = ":8080"
	MaxConcurrentJobs  = 4        // Limit concurrent image processing
	MaxFileSize        = 30 << 20 // 30MB per file
	LargeFileThreshold = 15 << 20 // 15MB - threshold for special handling
	ChunkSize          = 1 << 20  // 1MB chunks for large file processing
)

var (
	s3Uploader *manager.Uploader
	workerPool chan struct{} // Semaphore for limiting concurrent processing
)

// CompressionRequest represents an image conversion request
type CompressionRequest struct {
	File     multipart.File
	Header   *multipart.FileHeader
	Quality  float64
	Lossless bool
	MaxWidth int
}

// CompressionResponse contains the result of conversion
type CompressionResponse struct {
	WebPData []byte
	Err      error
}

func initAWS() {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("AWS_REGION")))
	if err != nil {
		log.Fatalf("unable to load AWS SDK config, %v", err)
	}
	s3Uploader = manager.NewUploader(s3.NewFromConfig(cfg))
}

func initWorkerPool() {
	// Create a buffered channel to limit concurrent workers
	workerPool = make(chan struct{}, MaxConcurrentJobs)
	for i := 0; i < MaxConcurrentJobs; i++ {
		workerPool <- struct{}{} // Fill the pool
	}
}

func acquireWorker() {
	<-workerPool // Take a worker from the pool
}

func releaseWorker() {
	workerPool <- struct{}{} // Return worker to the pool
}

func uploadToS3(ctx context.Context, data []byte, key string) (string, error) {
	upParams := &s3.PutObjectInput{
		Bucket:      aws.String(os.Getenv("AWS_BUCKET")),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("image/webp"),
	}
	_, err := s3Uploader.Upload(ctx, upParams)
	if err != nil {
		return "", err
	}
	url := "https://" + os.Getenv("AWS_BUCKET") + ".s3." + os.Getenv("AWS_REGION") + ".amazonaws.com/" + key
	return url, nil
}

func main() {
	// Set memory optimization
	debug.SetGCPercent(50)        // More aggressive garbage collection
	debug.SetMemoryLimit(1 << 30) // 1GB memory limit

	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found or failed to load")
	}

	initAWS()
	initWorkerPool()

	// Set up rate limiting
	rate, err := limiter.NewRateFromFormatted(RateLimit)
	if err != nil {
		log.Fatal(err)
	}
	store := memory.NewStore()
	rateLimiter := limiter.New(store, rate)
	rateLimitMiddleware := stdlib.NewMiddleware(rateLimiter)

	// Start memory monitoring
	go monitorMemory()

	// Create HTTP server with timeouts
	server := &http.Server{
		Addr:         Port,
		ReadTimeout:  ServerTimeout,
		WriteTimeout: ServerTimeout,
		Handler:      rateLimitMiddleware.Handler(initRouter()),
	}

	log.Printf("Server starting on %s with %d max concurrent jobs, max file size: %dMB",
		Port, MaxConcurrentJobs, MaxFileSize/(1024*1024))
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func monitorMemory() {
	for {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("Memory: Alloc=%dMB, TotalAlloc=%dMB, Sys=%dMB, NumGC=%d",
			bToMb(m.Alloc), bToMb(m.TotalAlloc), bToMb(m.Sys), m.NumGC)
		time.Sleep(30 * time.Second)
	}
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

func initRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/convert", convertHandler)
	mux.HandleFunc("/batch-convert", batchConvertHandler)
	mux.HandleFunc("/health", healthCheckHandler)
	mux.HandleFunc("/memory", memoryHandler)
	mux.HandleFunc("/limits", limitsHandler)
	mux.HandleFunc("/", healthCheckHandler)
	return mux
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func limitsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"max_file_size_mb":        MaxFileSize / (1024 * 1024),
		"max_upload_size_mb":      MaxUploadSize / (1024 * 1024),
		"large_file_threshold_mb": LargeFileThreshold / (1024 * 1024),
		"max_width":               MaxWidth,
		"max_concurrent_jobs":     MaxConcurrentJobs,
		"max_batch_files":         20,
		"server_timeout_seconds":  int(ServerTimeout.Seconds()),
	})
}

func memoryHandler(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"alloc_mb":       bToMb(m.Alloc),
		"total_alloc_mb": bToMb(m.TotalAlloc),
		"sys_mb":         bToMb(m.Sys),
		"num_gc":         m.NumGC,
		"goroutines":     runtime.NumGoroutine(),
	})
}

func convertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Parse request with file size validation
	req, err := parseCompressionRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer req.File.Close()

	// Acquire worker
	acquireWorker()
	defer releaseWorker()

	// Process image
	response := processImageOptimized(req)

	if response.Err != nil {
		http.Error(w, response.Err.Error(), http.StatusInternalServerError)
		return
	}

	// Upload to S3
	key := "uploads/" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".webp"
	s3url, err := uploadToS3(r.Context(), response.WebPData, key)
	if err != nil {
		http.Error(w, "Failed to upload to S3: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Immediately free memory
	response.WebPData = nil
	runtime.GC()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"data": true, "url": "` + s3url + `", "key": "` + key + `"}`))

	elapsed := time.Since(start)
	log.Printf("Conversion and upload took %s", elapsed)
}

func batchConvertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Parse multipart form with size limit
	if err := r.ParseMultipartForm(MaxUploadSize); err != nil {
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		return
	}

	// Get quality/lossless settings
	quality, err := strconv.ParseFloat(r.FormValue("quality"), 64)
	if err != nil || quality <= 0 {
		quality = DefaultQuality
	}
	lossless := r.FormValue("lossless") == "true"

	// Process files with limited concurrency
	files := r.MultipartForm.File["images"]

	// Validate total files and sizes
	if len(files) > 20 { // Limit batch size
		http.Error(w, "Too many files. Maximum 20 files per batch", http.StatusBadRequest)
		return
	}

	// Check total size and individual file sizes
	var totalSize int64
	for _, fh := range files {
		if fh.Size > MaxFileSize {
			http.Error(w, fmt.Sprintf("File %s too large: %d bytes (max: %d)",
				fh.Filename, fh.Size, MaxFileSize), http.StatusBadRequest)
			return
		}
		totalSize += fh.Size
	}

	if totalSize > MaxUploadSize {
		http.Error(w, fmt.Sprintf("Total upload size too large: %d bytes (max: %d)",
			totalSize, MaxUploadSize), http.StatusBadRequest)
		return
	}

	log.Printf("Processing batch of %d files, total size: %d bytes", len(files), totalSize)

	results := make([]map[string]string, len(files))
	var wg sync.WaitGroup

	for i, fileHeader := range files {
		wg.Add(1)
		go func(idx int, fh *multipart.FileHeader) {
			defer wg.Done()

			// Acquire worker (limits concurrency)
			acquireWorker()
			defer releaseWorker()

			file, err := fh.Open()
			if err != nil {
				log.Printf("Error opening file %s: %v", fh.Filename, err)
				return
			}
			defer file.Close()

			req := CompressionRequest{
				File:     file,
				Header:   fh,
				Quality:  quality,
				Lossless: lossless,
				MaxWidth: MaxWidth,
			}

			resp := processImageOptimized(&req)
			if resp.Err != nil {
				log.Printf("Error processing file %s: %v", fh.Filename, resp.Err)
				return
			}

			key := "uploads/" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".webp"
			s3url, err := uploadToS3(r.Context(), resp.WebPData, key)
			if err != nil {
				log.Printf("Error uploading file %s: %v", fh.Filename, err)
				return
			}

			results[idx] = map[string]string{"url": s3url, "key": key}

			// Free memory immediately after upload
			resp.WebPData = nil
		}(i, fileHeader)
	}

	wg.Wait()

	// Force garbage collection after batch processing
	runtime.GC()

	// Filter out any nil/empty results (failed conversions)
	finalResults := make([]map[string]string, 0, len(results))
	for _, res := range results {
		if res != nil && res["url"] != "" {
			finalResults = append(finalResults, res)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"data": true, "results": ` + marshalResults(finalResults) + `}`))

	elapsed := time.Since(start)
	log.Printf("Batch conversion and upload took %s", elapsed)
}

func marshalResults(results []map[string]string) string {
	b, _ := json.Marshal(results)
	return string(b)
}

func parseCompressionRequest(r *http.Request) (*CompressionRequest, error) {
	// Parse quality
	quality, err := strconv.ParseFloat(r.FormValue("quality"), 64)
	if err != nil || quality <= 0 {
		quality = DefaultQuality
	}

	// Parse lossless flag
	lossless := r.FormValue("lossless") == "true"

	// Parse image file
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, err
	}

	// Validate file size
	if header.Size > MaxFileSize {
		return nil, fmt.Errorf("file too large: %d bytes (max: %d)", header.Size, MaxFileSize)
	}

	return &CompressionRequest{
		File:     file,
		Header:   header,
		Quality:  quality,
		Lossless: lossless,
		MaxWidth: MaxWidth,
	}, nil
}

func processImageOptimized(req *CompressionRequest) CompressionResponse {
	fileSize := req.Header.Size

	// For large files, use special handling
	if fileSize > LargeFileThreshold {
		return processLargeImage(req)
	}

	return processStandardImage(req)
}

func processLargeImage(req *CompressionRequest) CompressionResponse {
	log.Printf("Processing large file: %s (%d bytes)", req.Header.Filename, req.Header.Size)

	// Create a temporary file to avoid memory spikes
	tmpFile, err := os.CreateTemp("", "large-image-*.tmp")
	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to create temp file: %v", err)}
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Copy file data to temp file in chunks
	written, err := io.CopyBuffer(tmpFile, req.File, make([]byte, ChunkSize))
	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to copy to temp file: %v", err)}
	}

	log.Printf("Copied %d bytes to temp file", written)

	// Reopen temp file for reading
	tmpFile.Seek(0, 0)

	// Try to decode with config first to get dimensions without loading full image
	var config image.Config
	var decodeErr error

	contentType := req.Header.Header.Get("Content-Type")
	switch contentType {
	case "image/png":
		config, decodeErr = png.DecodeConfig(tmpFile)
	default:
		config, decodeErr = jpeg.DecodeConfig(tmpFile)
	}

	if decodeErr != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to decode image config: %v", decodeErr)}
	}

	log.Printf("Image dimensions: %dx%d", config.Width, config.Height)

	// Calculate smart resize ratio for extremely large images
	targetWidth := req.MaxWidth
	resizeRatio := 1.0

	if config.Width > req.MaxWidth {
		resizeRatio = float64(req.MaxWidth) / float64(config.Width)
		log.Printf("Will resize by ratio: %.3f", resizeRatio)
	}

	// Check if image is too large in dimensions even after planned resize
	estimatedMemory := int64(config.Width * config.Height * 4) // 4 bytes per pixel
	maxMemoryForLargeFiles := int64(800 * 1024 * 1024)         // 800MB limit for large files

	// If still too large, calculate a more aggressive resize
	if estimatedMemory > maxMemoryForLargeFiles {
		// Calculate maximum dimensions we can handle
		maxPixels := maxMemoryForLargeFiles / 4 // 4 bytes per pixel
		maxDimension := int(math.Sqrt(float64(maxPixels)))

		if config.Width > maxDimension || config.Height > maxDimension {
			// Use the smaller of current target or safe dimension
			if maxDimension < targetWidth {
				targetWidth = maxDimension
				resizeRatio = float64(maxDimension) / float64(config.Width)
				log.Printf("Extremely large image detected, using aggressive resize ratio: %.3f (target width: %d)",
					resizeRatio, targetWidth)
			}
		} else {
			return CompressionResponse{Err: fmt.Errorf("image dimensions too large: %dx%d (estimated %dMB, max: %dMB)",
				config.Width, config.Height, estimatedMemory/(1024*1024), maxMemoryForLargeFiles/(1024*1024))}
		}
	}

	// Reset file pointer and decode the actual image
	tmpFile.Seek(0, 0)

	var img image.Image
	switch contentType {
	case "image/png":
		img, err = png.Decode(tmpFile)
	default:
		img, err = jpeg.Decode(tmpFile)
	}

	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to decode large image: %v", err)}
	}

	// Resize if needed
	bounds := img.Bounds()
	currentWidth := bounds.Dx()

	if currentWidth > targetWidth {
		log.Printf("Resizing large image from %dx%d to target width %d", currentWidth, bounds.Dy(), targetWidth)
		img = imaging.Resize(img, targetWidth, 0, imaging.Lanczos)
		runtime.GC() // Force GC after resize
		log.Printf("Resize completed, new dimensions: %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Convert to WebP
	var buf bytes.Buffer
	options := &webp.Options{
		Quality:  float32(req.Quality),
		Lossless: req.Lossless,
	}

	if err := webp.Encode(&buf, img, options); err != nil {
		return CompressionResponse{Err: fmt.Errorf("webp encode error for large image: %v", err)}
	}

	// Clear memory
	img = nil
	runtime.GC()

	log.Printf("Successfully processed large image: %s (final size: %d bytes)", req.Header.Filename, len(buf.Bytes()))
	return CompressionResponse{WebPData: buf.Bytes()}
}

func processStandardImage(req *CompressionRequest) CompressionResponse {
	// Create a limited reader to prevent excessive memory usage
	limitedReader := &io.LimitedReader{R: req.File, N: MaxFileSize}

	// Decode image based on content type
	var img image.Image
	var err error

	contentType := req.Header.Header.Get("Content-Type")
	switch contentType {
	case "image/png":
		img, err = png.Decode(limitedReader)
	default: // Default to JPEG
		img, err = jpeg.Decode(limitedReader)
	}

	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("decode error: %v", err)}
	}

	// Check image dimensions and memory requirements
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	// Estimate memory usage (4 bytes per pixel for RGBA)
	estimatedMemory := width * height * 4
	if estimatedMemory > 100*1024*1024 { // 100MB limit for standard files
		return CompressionResponse{Err: fmt.Errorf("image too large: %dx%d pixels (estimated %dMB)",
			width, height, estimatedMemory/(1024*1024))}
	}

	// Resize if needed
	if width > req.MaxWidth {
		img = imaging.Resize(img, req.MaxWidth, 0, imaging.Lanczos)
		runtime.GC()
	}

	// Convert to WebP
	var buf bytes.Buffer
	options := &webp.Options{
		Quality:  float32(req.Quality),
		Lossless: req.Lossless,
	}

	if err := webp.Encode(&buf, img, options); err != nil {
		return CompressionResponse{Err: fmt.Errorf("webp encode error: %v", err)}
	}

	// Clear memory
	img = nil
	runtime.GC()

	return CompressionResponse{WebPData: buf.Bytes()}
}
