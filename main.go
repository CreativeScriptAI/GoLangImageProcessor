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

// Optimized settings for speed
const (
	MaxUploadSize      = 30 << 20 // 30MB (reduced)
	DefaultQuality     = 70       // Lower quality for speed
	MaxWidth           = 1920     // Reduced max width
	RateLimit          = "150-M"  // Higher rate limit
	ServerTimeout      = 45 * time.Second
	Port               = ":8080"
	MaxConcurrentJobs  = 8          // Increased concurrency
	MaxFileSize        = 15 << 20   // 15MB per file (reduced)
	LargeFileThreshold = 8 << 20    // 8MB threshold
	ChunkSize          = 512 * 1024 // 512KB chunks
)

var (
	s3Uploader *manager.Uploader
	workerPool chan struct{}

	// Simple buffer pool for performance
	bufferPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, ChunkSize)
			return &b
		},
	}
)

type CompressionRequest struct {
	File     multipart.File
	Header   *multipart.FileHeader
	Quality  float64
	Lossless bool
	MaxWidth int
}

type CompressionResponse struct {
	WebPData []byte
	Err      error
}

func initAWS() {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(os.Getenv("AWS_REGION")))
	if err != nil {
		log.Fatalf("unable to load AWS SDK config, %v", err)
	}

	// Optimized uploader settings
	s3Uploader = manager.NewUploader(s3.NewFromConfig(cfg), func(u *manager.Uploader) {
		u.PartSize = 5 * 1024 * 1024 // 5MB parts for faster upload
		u.Concurrency = 3            // Parallel uploads
	})
}

func initWorkerPool() {
	workerPool = make(chan struct{}, MaxConcurrentJobs)
	for i := 0; i < MaxConcurrentJobs; i++ {
		workerPool <- struct{}{}
	}
}

func acquireWorker() {
	<-workerPool
}

func releaseWorker() {
	workerPool <- struct{}{}
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
	// Simple optimizations
	debug.SetGCPercent(75)
	debug.SetMemoryLimit(1 << 30)

	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found or failed to load")
	}

	initAWS()
	initWorkerPool()

	rate, err := limiter.NewRateFromFormatted(RateLimit)
	if err != nil {
		log.Fatal(err)
	}
	store := memory.NewStore()
	rateLimiter := limiter.New(store, rate)
	rateLimitMiddleware := stdlib.NewMiddleware(rateLimiter)

	go monitorMemory()

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

	req, err := parseCompressionRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer req.File.Close()

	acquireWorker()
	defer releaseWorker()

	response := processImageOptimized(req)
	if response.Err != nil {
		http.Error(w, response.Err.Error(), http.StatusInternalServerError)
		return
	}

	key := "uploads/" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".webp"
	s3url, err := uploadToS3(r.Context(), response.WebPData, key)
	if err != nil {
		http.Error(w, "Failed to upload to S3: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Quick cleanup
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

	if err := r.ParseMultipartForm(MaxUploadSize); err != nil {
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		return
	}

	quality, err := strconv.ParseFloat(r.FormValue("quality"), 64)
	if err != nil || quality <= 0 {
		quality = DefaultQuality
	}
	lossless := r.FormValue("lossless") == "true"

	files := r.MultipartForm.File["images"]

	if len(files) > 20 {
		http.Error(w, "Too many files. Maximum 20 files per batch", http.StatusBadRequest)
		return
	}

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
			resp.WebPData = nil
		}(i, fileHeader)
	}

	wg.Wait()
	runtime.GC()

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
	quality, err := strconv.ParseFloat(r.FormValue("quality"), 64)
	if err != nil || quality <= 0 {
		quality = DefaultQuality
	}

	lossless := r.FormValue("lossless") == "true"

	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, err
	}

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

	if fileSize > LargeFileThreshold {
		return processLargeImageFast(req)
	}

	return processStandardImageFast(req)
}

func processLargeImageFast(req *CompressionRequest) CompressionResponse {
	log.Printf("Processing large file: %s (%d bytes)", req.Header.Filename, req.Header.Size)

	tmpFile, err := os.CreateTemp("", "large-image-*.tmp")
	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to create temp file: %v", err)}
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Use buffer pool
	bufferPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufferPtr)

	written, err := io.CopyBuffer(tmpFile, req.File, *bufferPtr)
	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to copy to temp file: %v", err)}
	}

	log.Printf("Copied %d bytes to temp file", written)

	tmpFile.Seek(0, 0)

	var config image.Config
	contentType := req.Header.Header.Get("Content-Type")
	switch contentType {
	case "image/png":
		config, err = png.DecodeConfig(tmpFile)
	default:
		config, err = jpeg.DecodeConfig(tmpFile)
	}

	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("failed to decode image config: %v", err)}
	}

	log.Printf("Image dimensions: %dx%d", config.Width, config.Height)

	targetWidth := req.MaxWidth
	if config.Width > req.MaxWidth {
		log.Printf("Will resize from %d to %d", config.Width, req.MaxWidth)
	}

	// Simple memory check
	estimatedMemory := int64(config.Width * config.Height * 4)
	maxMemoryForLargeFiles := int64(500 * 1024 * 1024) // 500MB

	if estimatedMemory > maxMemoryForLargeFiles {
		maxPixels := maxMemoryForLargeFiles / 4
		maxDimension := int(math.Sqrt(float64(maxPixels)))
		if maxDimension < targetWidth {
			targetWidth = maxDimension
			log.Printf("Very large image detected, resizing to %d", targetWidth)
		}
	}

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

	bounds := img.Bounds()
	currentWidth := bounds.Dx()

	if currentWidth > targetWidth {
		log.Printf("Resizing large image from %dx%d to target width %d", currentWidth, bounds.Dy(), targetWidth)
		// Use faster resize algorithm
		img = imaging.Resize(img, targetWidth, 0, imaging.Box)
		runtime.GC()
		log.Printf("Resize completed")
	}

	var buf bytes.Buffer
	options := &webp.Options{
		Quality:  float32(req.Quality),
		Lossless: req.Lossless,
	}

	if err := webp.Encode(&buf, img, options); err != nil {
		return CompressionResponse{Err: fmt.Errorf("webp encode error for large image: %v", err)}
	}

	img = nil
	runtime.GC()

	log.Printf("Successfully processed large image: %s (final size: %d bytes)", req.Header.Filename, len(buf.Bytes()))
	return CompressionResponse{WebPData: buf.Bytes()}
}

func processStandardImageFast(req *CompressionRequest) CompressionResponse {
	// Use buffer pool
	bufferPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufferPtr)

	var buf bytes.Buffer
	_, err := io.CopyBuffer(&buf, req.File, *bufferPtr)
	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("read error: %v", err)}
	}

	var img image.Image
	reader := bytes.NewReader(buf.Bytes())

	contentType := req.Header.Header.Get("Content-Type")
	switch contentType {
	case "image/png":
		img, err = png.Decode(reader)
	default:
		img, err = jpeg.Decode(reader)
	}

	if err != nil {
		return CompressionResponse{Err: fmt.Errorf("decode error: %v", err)}
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	// Simple memory check
	estimatedMemory := width * height * 4
	if estimatedMemory > 100*1024*1024 { // 100MB limit
		return CompressionResponse{Err: fmt.Errorf("image too large: %dx%d pixels", width, height)}
	}

	if width > req.MaxWidth {
		// Use faster resize algorithm
		img = imaging.Resize(img, req.MaxWidth, 0, imaging.Box)
		runtime.GC()
	}

	var encodeBuf bytes.Buffer
	options := &webp.Options{
		Quality:  float32(req.Quality),
		Lossless: req.Lossless,
	}

	if err := webp.Encode(&encodeBuf, img, options); err != nil {
		return CompressionResponse{Err: fmt.Errorf("webp encode error: %v", err)}
	}

	img = nil
	runtime.GC()

	return CompressionResponse{WebPData: encodeBuf.Bytes()}
}
