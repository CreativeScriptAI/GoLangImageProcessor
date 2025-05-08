package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/middleware/stdlib"
	"github.com/ulule/limiter/v3/drivers/store/memory"
)

// Configurable settings
const (
	MaxUploadSize  = 32 << 20 // 32MB
	DefaultQuality = 80
	MaxWidth       = 2500    // Maximum width before resizing
	RateLimit      = "100-M" // 100 requests per minute
	ServerTimeout  = 30 * time.Second
	Port           = ":8080"
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

func main() {
	// Set up rate limiting
	rate, err := limiter.NewRateFromFormatted(RateLimit)
	if err != nil {
		log.Fatal(err)
	}
	store := memory.NewStore()
	rateLimiter := limiter.New(store, rate)
	rateLimitMiddleware := stdlib.NewMiddleware(rateLimiter)

	// Create HTTP server with timeouts
	server := &http.Server{
		Addr:         Port,
		ReadTimeout:  ServerTimeout,
		WriteTimeout: ServerTimeout,
		Handler:      rateLimitMiddleware.Handler(initRouter()),
	}

	log.Printf("Server starting on %s", Port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func initRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/convert", convertHandler)
	mux.HandleFunc("/batch-convert", batchConvertHandler)
	mux.HandleFunc("/", healthCheckHandler)
	return mux
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func convertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	//
	start := time.Now()

	// Parse request
	req, err := parseCompressionRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer req.File.Close()

	// Process image
	response := processImage(req)

	if response.Err != nil {
		http.Error(w, response.Err.Error(), http.StatusInternalServerError)
		return
	}

	// Return WebP
	w.Header().Set("Content-Type", "image/webp")
	w.Write(response.WebPData)

	//
	elapsed := time.Since(start)
	log.Printf("Conversion took %s", elapsed)
}

func batchConvertHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	//
	start := time.Now()

	// Parse multipart form
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

	// Process files in parallel
	files := r.MultipartForm.File["images"]
	responses := make(chan CompressionResponse, len(files))
	var wg sync.WaitGroup

	for _, fileHeader := range files {
		wg.Add(1)
		go func(fh *multipart.FileHeader) {
			defer wg.Done()

			file, err := fh.Open()
			if err != nil {
				responses <- CompressionResponse{Err: err}
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

			responses <- processImage(&req)
		}(fileHeader)
	}

	// Wait for all goroutines to finish
	go func() {
		wg.Wait()
		close(responses)
	}()

	// Create multipart response
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", mw.FormDataContentType())

	for resp := range responses {
		if resp.Err != nil {
			continue // Skip failed conversions
		}

		part, err := mw.CreateFormFile("images", "converted.webp")
		if err != nil {
			continue
		}
		part.Write(resp.WebPData)
	}
	mw.Close()
	elapsed := time.Since(start)
	log.Printf("Batch conversion took %s", elapsed)
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
	file, header, err := r.FormFile("image")
	if err != nil {
		return nil, err
	}

	return &CompressionRequest{
		File:     file,
		Header:   header,
		Quality:  quality,
		Lossless: lossless,
		MaxWidth: MaxWidth,
	}, nil
}

func processImage(req *CompressionRequest) CompressionResponse {
	// Decode image based on content type
	var img image.Image
	var err error

	contentType := req.Header.Header.Get("Content-Type")
	switch contentType {
	case "image/png":
		img, err = png.Decode(req.File)
	default: // Default to JPEG
		img, err = jpeg.Decode(req.File)
	}

	if err != nil {
		return CompressionResponse{Err: err}
	}

	// Resize if needed
	bounds := img.Bounds()
	if bounds.Dx() > req.MaxWidth {
		img = imaging.Resize(img, req.MaxWidth, 0, imaging.Lanczos)
	}

	// Convert to WebP
	var buf bytes.Buffer
	options := &webp.Options{
		Quality:  float32(req.Quality),
		Lossless: req.Lossless,
	}

	if err := webp.Encode(&buf, img, options); err != nil {
		return CompressionResponse{Err: err}
	}

	return CompressionResponse{WebPData: buf.Bytes()}
}
