package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// These get injected at build time:
	defaultUser  string
	defaultToken string
)

const maxSem = 8

type chunkResult struct {
	ETag  string
	Index int
	Err   error
}

func main() {
	// URL flag
	// Flags
	userFlag := flag.String("user", defaultUser, "Username (overrides build-time default)")
	tokenFlag := flag.String("token", defaultToken, "Auth token (overrides build-time default)")
	baseURL := flag.String("url", "https://transfer.atlassian.com",
		"Base API URL (e.g. https://api.example.com)")
	flag.Parse()

	if *userFlag == "" || *tokenFlag == "" {
		fmt.Fprintln(os.Stderr,
			"Error: missing user or token. Provide via build-time -ldflags or -user/-token flags.")
		os.Exit(1)
	} else {
		defaultUser = *userFlag
		defaultToken = *tokenFlag
	}

	// Positional args
	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] ISSUE-KEY FILEPATH\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}
	issueKey := args[0]
	filePath := args[1]

	if defaultUser == "" || defaultToken == "" {
		fmt.Fprintln(os.Stderr, "Error: user/token not set—build with -ldflags to inject them.")
		os.Exit(1)
	}

	uploader := NewFileUploader(filePath, issueKey, defaultUser, defaultToken, *baseURL)
	if err := uploader.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Successfully uploaded %s to %s\n", filePath, issueKey)
}

type FileUploader struct {
	FilePath  string
	IssueKey  string
	User      string
	Token     string
	BaseURL   string
	Client    *http.Client
	Semaphore chan struct{}
}

func NewFileUploader(fp, ik, u, t, url string) *FileUploader {
	return &FileUploader{
		FilePath:  fp,
		IssueKey:  ik,
		User:      u,
		Token:     t,
		BaseURL:   url,
		Client:    &http.Client{Timeout: 30 * time.Second},
		Semaphore: make(chan struct{}, maxSem),
	}
}

func (fu *FileUploader) Run() error {
	// Stat file to get size
	fi, err := os.Stat(fu.FilePath)
	if err != nil {
		return err
	}
	size := fi.Size()
	blockSize := getBlockSize(size)
	totalChunks := int((size / blockSize) + 1)

	// 1) Create upload session
	uploadID, err := fu.createUpload()
	if err != nil {
		return err
	}

	// 2) Progress bar
	p := mpb.New()
	bar := p.AddBar(int64(totalChunks),
		mpb.PrependDecorators(
			decor.Name("Uploading:", decor.WC{W: 10}),
			decor.CountersNoUnit("%d / %d", decor.WC{W: 12}),
		),
		mpb.AppendDecorators(decor.Percentage()),
	)

	file, err := os.Open(fu.FilePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// 3) Spawn workers
	var wg sync.WaitGroup
	results := make(chan chunkResult, totalChunks)

	idx := 0
	for {
		buf := make([]byte, blockSize)
		n, readErr := file.Read(buf)
		if n == 0 {
			break
		}
		buf = buf[:n]

		wg.Add(1)
		fu.Semaphore <- struct{}{} // acquire
		go func(index int, chunk []byte) {
			defer wg.Done()
			defer func() { <-fu.Semaphore }() // release

			etag, err := fu.processChunk(chunk, index+1, uploadID)
			results <- chunkResult{ETag: etag, Index: index + 1, Err: err}
			bar.Increment()
		}(idx, buf)

		idx++
		if readErr == io.EOF {
			break
		} else if readErr != nil {
			return readErr
		}
	}

	// 4) Collect results
	go func() {
		wg.Wait()
		close(results)
	}()

	var chunks []chunkResult
	for res := range results {
		if res.Err != nil {
			return res.Err
		}
		chunks = append(chunks, res)
	}

	// Sort by Index
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].Index < chunks[j].Index
	})

	// Build list of ETags
	etags := make([]string, len(chunks))
	for i, c := range chunks {
		etags[i] = c.ETag
	}

	// 5) Finalize upload
	if err := fu.createFileChunked(etags, uploadID); err != nil {
		return err
	}

	p.Wait()
	return nil
}

func (fu *FileUploader) createUpload() (string, error) {
	url := fmt.Sprintf("%s/api/upload/%s/create", fu.BaseURL, fu.IssueKey)
	req, _ := http.NewRequest("POST", url, nil)
	req.SetBasicAuth(fu.User, fu.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := fu.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return "", fmt.Errorf("authentication failed")
	}
	if resp.StatusCode != http.StatusCreated {
		rt, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create upload: status %d", resp.StatusCode, string(rt))
	}

	var body struct {
		UploadId string `json:"uploadId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.UploadId, nil
}

func (fu *FileUploader) processChunk(buf []byte, partNumber int, uploadID string) (string, error) {
	etag := generateETag(buf)
	exists, err := fu.checkIfChunkExists(etag, uploadID)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := fu.uploadChunk(etag, buf, partNumber, uploadID); err != nil {
			return "", err
		}
	}
	return etag, nil
}

func (fu *FileUploader) checkIfChunkExists(etag, uploadID string) (bool, error) {
	var exists bool
	op := func() error {
		url := fmt.Sprintf("%s/api/upload/%s/chunk/probe?uploadId=%s",
			fu.BaseURL, fu.IssueKey, uploadID)
		payload := map[string]interface{}{
			"chunks": getChunksJSON([]string{etag}),
		}
		body, _ := json.Marshal(payload)

		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.SetBasicAuth(fu.User, fu.Token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := fu.Client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode == 401 {
			return backoff.Permanent(fmt.Errorf("authentication failed"))
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("probe status %d", resp.StatusCode)
		}

		var respJSON struct {
			Data struct {
				Results map[string]struct {
					Exists bool `json:"exists"`
				} `json:"results"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&respJSON); err != nil {
			return err
		}
		// JSON key is "sha256-"+etag
		key := "sha256-" + etag
		exists = respJSON.Data.Results[key].Exists
		return nil
	}

	backoffCfg := backoff.NewExponentialBackOff()
	if err := backoff.Retry(op, backoffCfg); err != nil {
		return false, err
	}
	return exists, nil
}

func (fu *FileUploader) uploadChunk(etag string, chunk []byte, partNumber int, uploadID string) error {
	op := func() error {
		url := fmt.Sprintf("%s/api/upload/%s/chunk/%s?uploadId=%s&partNumber=%d",
			fu.BaseURL, fu.IssueKey, etag, uploadID, partNumber)

		buf := &bytes.Buffer{}
		writer := multipart.NewWriter(buf)
		part, _ := writer.CreateFormFile("chunk", filepath.Base(fu.FilePath))
		io.Copy(part, bytes.NewReader(chunk))
		writer.Close()

		req, _ := http.NewRequest("POST", url, buf)
		req.SetBasicAuth(fu.User, fu.Token)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		resp, err := fu.Client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode == 401 {
			return backoff.Permanent(fmt.Errorf("authentication failed"))
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("upload chunk status %d", resp.StatusCode)
		}
		return nil
	}

	backoffCfg := backoff.NewExponentialBackOff()
	return backoff.Retry(op, backoffCfg)
}

func (fu *FileUploader) createFileChunked(etags []string, uploadID string) error {
	op := func() error {
		url := fmt.Sprintf("%s/api/upload/%s/file/chunked?uploadId=%s",
			fu.BaseURL, fu.IssueKey, uploadID)

		payload := map[string]interface{}{
			"chunks":   getChunksJSON(etags),
			"name":     filepath.Base(fu.FilePath),
			"mimeType": mime.TypeByExtension(filepath.Ext(fu.FilePath)),
		}
		body, _ := json.Marshal(payload)

		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.SetBasicAuth(fu.User, fu.Token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := fu.Client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode == 401 {
			return backoff.Permanent(fmt.Errorf("authentication failed"))
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("finalize status %d", resp.StatusCode)
		}
		return nil
	}

	backoffCfg := backoff.NewExponentialBackOff()
	return backoff.Retry(op, backoffCfg)
}

// Helpers

// getBlockSize mirrors Python's FileService.get_block_size exactly.
func getBlockSize(fileSize int64) int64 {
	mb := float64(fileSize) / (1024 * 1024)
	blocks := math.Ceil(mb / 10000)
	var cnt float64
	switch {
	case blocks < 5:
		cnt = 5
	case blocks < 50:
		cnt = 50
	case blocks < 100:
		cnt = 100
	default:
		cnt = 210
	}
	return int64(cnt * 1024 * 1024)
}

// generateETag mirrors hashlib.sha256 + "-" + len(buf)
func generateETag(buf []byte) string {
	sum := sha256.Sum256(buf)
	h := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%d", h, len(buf))
}

// getChunksJSON builds the exact JSON body from etag strings.
func getChunksJSON(etags []string) []map[string]string {
	out := make([]map[string]string, len(etags))
	for i, et := range etags {
		parts := strings.SplitN(et, "-", 2)
		out[i] = map[string]string{
			"hash": parts[0],
			"size": parts[1],
		}
	}
	return out
}
