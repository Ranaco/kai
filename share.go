package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	exitCodeSuccess         = 0
	exitCodeUsage           = 2
	exitCodeSourceError     = 3
	exitCodeUploadError     = 4
	exitCodeSafetyError     = 5
	exitCodeTimeoutCanceled = 6
)

var errMaxSizeExceeded = errors.New("max size exceeded")

type shareConfig struct {
	From           string
	LocalFile      string
	Provider       string
	To             string
	Method         string
	Headers        []string
	Cookies        []string
	Timeout        time.Duration
	ConnectTimeout time.Duration
	MaxSize        int64
	AllowDomains   []string
	DenyPrivateIP  bool
	Progress       bool
	Output         string
	Verbose        bool
}

type shareResult struct {
	ShareURL   string `json:"share_url"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Source     string `json:"source"`
	Provider   string `json:"provider"`
}

type sourceMeta struct {
	ContentLength int64
	ContentType   string
	Filename      string
	SourceLabel   string
}

type shareError struct {
	Code     string
	Message  string
	ExitCode int
	Err      error
}

func (e *shareError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

type repeatableValue []string

func (r *repeatableValue) String() string {
	return strings.Join(*r, ",")
}

func (r *repeatableValue) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func runShare(args []string) int {
	leadingPositionals := make([]string, 0, 2)
	normalizedArgs := args
	for len(normalizedArgs) > 0 && len(leadingPositionals) < 2 {
		if strings.HasPrefix(normalizedArgs[0], "-") {
			break
		}
		leadingPositionals = append(leadingPositionals, normalizedArgs[0])
		normalizedArgs = normalizedArgs[1:]
	}

	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		printShareUsage(fs)
	}

	var headers repeatableValue
	var cookies repeatableValue
	var allowDomains repeatableValue

	from := fs.String("from", "", "Source URL")
	localFile := fs.String("file", "", "Local file path source")
	provider := fs.String("provider", "", "Upload provider: catbox, generic_put, or generic_multipart")
	to := fs.String("to", "", "Upload endpoint URL (required for generic providers)")
	method := fs.String("method", http.MethodGet, "Source method: GET or POST")
	timeout := fs.Duration("timeout", 15*time.Minute, "Total timeout")
	connectTimeout := fs.Duration("connect-timeout", 15*time.Second, "Connection timeout")
	maxSize := fs.String("max-size", "2GB", "Maximum transferable size")
	denyPrivateIP := fs.Bool("deny-private-ip", true, "Block private/loopback/link-local target IPs")
	progress := fs.Bool("progress", true, "Show progress")
	output := fs.String("output", "text", "Output format: text or json")
	verbose := fs.Bool("verbose", false, "Verbose logging")

	fs.Var(&headers, "header", "Source header, repeatable (Key: Value)")
	fs.Var(&cookies, "cookie", "Source cookie, repeatable (k=v)")
	fs.Var(&allowDomains, "allow-domain", "Allowed source domain, repeatable")

	if len(args) == 0 {
		printShareUsage(fs)
		return exitCodeSuccess
	}

	if err := fs.Parse(normalizedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitCodeSuccess
		}
		return exitCodeUsage
	}

	positionals := append([]string{}, leadingPositionals...)
	positionals = append(positionals, fs.Args()...)
	if len(positionals) > 2 {
		printShareError(*output, &shareError{
			Code:     "INVALID_ARGS",
			Message:  "too many positional arguments; expected: kai share <from-url> <provider>",
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}
	if *from == "" && *localFile == "" && len(positionals) >= 1 {
		*from = positionals[0]
	}
	if *provider == "" && len(positionals) >= 2 {
		*provider = positionals[1]
	}

	if *from == "" && *localFile == "" {
		printShareError(*output, &shareError{
			Code:     "INVALID_ARGS",
			Message:  "one source is required: --from <url> or --file <path> (or positional source)",
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}
	if *from != "" && *localFile != "" {
		printShareError(*output, &shareError{
			Code:     "INVALID_ARGS",
			Message:  "use only one source: --from or --file",
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}
	if *provider == "" {
		printShareError(*output, &shareError{
			Code:     "INVALID_ARGS",
			Message:  "--provider is required (or use positional: kai share <source> <provider>)",
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}

	maxSizeBytes, err := parseSize(*maxSize)
	if err != nil {
		printShareError(*output, &shareError{
			Code:     "INVALID_MAX_SIZE",
			Message:  fmt.Sprintf("invalid --max-size: %v", err),
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}

	if *output != "text" && *output != "json" {
		printShareError(*output, &shareError{
			Code:     "INVALID_OUTPUT",
			Message:  "--output must be text or json",
			ExitCode: exitCodeUsage,
		})
		return exitCodeUsage
	}

	cfg := shareConfig{
		From:           *from,
		LocalFile:      *localFile,
		Provider:       strings.ToLower(*provider),
		To:             *to,
		Method:         strings.ToUpper(*method),
		Headers:        headers,
		Cookies:        cookies,
		Timeout:        *timeout,
		ConnectTimeout: *connectTimeout,
		MaxSize:        maxSizeBytes,
		AllowDomains:   allowDomains,
		DenyPrivateIP:  *denyPrivateIP,
		Progress:       *progress,
		Output:         *output,
		Verbose:        *verbose,
	}

	rootCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignal()

	ctx := rootCtx
	cancelTimeout := func() {}
	if cfg.Timeout > 0 {
		ctxWithTimeout, cancel := context.WithTimeout(rootCtx, cfg.Timeout)
		ctx = ctxWithTimeout
		cancelTimeout = cancel
	}
	defer cancelTimeout()

	started := time.Now()
	res, runErr := executeShare(ctx, cfg)
	if runErr != nil {
		se := classifyShareError(runErr)
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			se.ExitCode = exitCodeTimeoutCanceled
			se.Code = "TIMEOUT_OR_CANCELED"
			se.Message = runErr.Error()
		}
		printShareError(cfg.Output, se)
		return se.ExitCode
	}

	res.DurationMS = time.Since(started).Milliseconds()
	printShareSuccess(cfg.Output, res)
	return exitCodeSuccess
}

func printShareUsage(fs *flag.FlagSet) {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  kai share --from <url> --provider <provider> [flags]")
	fmt.Fprintln(os.Stderr, "  kai share --file <path> --provider <provider> [flags]")
	fmt.Fprintln(os.Stderr, "  kai share <source> <provider> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  kai share \"https://example.com/file.zip\" catbox")
	fmt.Fprintln(os.Stderr, "  kai share \"/tmp/report.pdf\" catbox")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Flags:")
	fs.PrintDefaults()
}

func executeShare(ctx context.Context, cfg shareConfig) (shareResult, error) {
	if cfg.Method != http.MethodGet && cfg.Method != http.MethodPost {
		return shareResult{}, &shareError{
			Code:     "INVALID_METHOD",
			Message:  "--method must be GET or POST",
			ExitCode: exitCodeUsage,
		}
	}
	if cfg.Provider != "generic_put" && cfg.Provider != "generic_multipart" && cfg.Provider != "catbox" {
		return shareResult{}, &shareError{
			Code:     "INVALID_PROVIDER",
			Message:  "--provider must be catbox, generic_put, or generic_multipart",
			ExitCode: exitCodeUsage,
		}
	}
	if cfg.Provider != "catbox" && cfg.To == "" {
		return shareResult{}, &shareError{
			Code:     "MISSING_UPLOAD_ENDPOINT",
			Message:  "--to is required for generic providers",
			ExitCode: exitCodeUsage,
		}
	}

	if cfg.Provider != "catbox" {
		uploadURL, err := url.Parse(cfg.To)
		if err != nil {
			return shareResult{}, &shareError{
				Code:     "INVALID_UPLOAD_URL",
				Message:  fmt.Sprintf("invalid upload URL: %v", err),
				ExitCode: exitCodeUsage,
			}
		}
		if uploadURL.Scheme != "http" && uploadURL.Scheme != "https" {
			return shareResult{}, &shareError{
				Code:     "INVALID_UPLOAD_URL",
				Message:  "upload URL must use http or https",
				ExitCode: exitCodeUsage,
			}
		}
	}

	sourceClient := &http.Client{
		Transport: newSafeTransport(cfg, true),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("source redirect limit exceeded")
			}
			return validateSourceURL(req.URL, cfg)
		},
	}
	uploadClient := &http.Client{
		Transport: newSafeTransport(cfg, false),
	}

	meta, baseReader, err := openSource(ctx, sourceClient, cfg)
	if err != nil {
		return shareResult{}, err
	}
	defer baseReader.Close()

	if cfg.MaxSize > 0 && meta.ContentLength > cfg.MaxSize {
		return shareResult{}, &shareError{
			Code:     "SIZE_LIMIT_EXCEEDED",
			Message:  fmt.Sprintf("source size %d exceeds max-size %d", meta.ContentLength, cfg.MaxSize),
			ExitCode: exitCodeSafetyError,
		}
	}

	var copiedBytes atomic.Int64
	var sourceReader io.Reader = baseReader
	if cfg.MaxSize > 0 {
		sourceReader = &maxSizeReader{r: sourceReader, limit: cfg.MaxSize}
	}
	sourceReader = &countingReader{
		r: sourceReader,
		onRead: func(n int) {
			copiedBytes.Add(int64(n))
		},
	}

	stopProgress := startProgressPrinter(ctx, cfg, &copiedBytes)
	defer stopProgress()

	shareURL, err := uploadFromSource(ctx, cfg, uploadClient, meta, sourceReader)
	if err != nil {
		if errors.Is(err, errMaxSizeExceeded) {
			return shareResult{}, &shareError{
				Code:     "SIZE_LIMIT_EXCEEDED",
				Message:  fmt.Sprintf("stream exceeded max-size %d", cfg.MaxSize),
				ExitCode: exitCodeSafetyError,
			}
		}
		return shareResult{}, err
	}

	return shareResult{
		ShareURL: shareURL,
		Bytes:    copiedBytes.Load(),
		Source:   meta.SourceLabel,
		Provider: cfg.Provider,
	}, nil
}

func openSource(ctx context.Context, client *http.Client, cfg shareConfig) (sourceMeta, io.ReadCloser, error) {
	if cfg.LocalFile != "" {
		return openLocalSource(cfg.LocalFile)
	}

	sourceURL, err := url.Parse(cfg.From)
	if err != nil {
		return sourceMeta{}, nil, &shareError{
			Code:     "INVALID_SOURCE_URL",
			Message:  fmt.Sprintf("invalid source URL: %v", err),
			ExitCode: exitCodeUsage,
		}
	}
	if sourceURL.Scheme != "http" && sourceURL.Scheme != "https" {
		// Positional source without scheme is treated as a local file path.
		return openLocalSource(cfg.From)
	}
	if err := validateSourceURL(sourceURL, cfg); err != nil {
		return sourceMeta{}, nil, err
	}

	resp, err := openSourceWithRetry(ctx, client, cfg, sourceURL)
	if err != nil {
		return sourceMeta{}, nil, err
	}

	return sourceMeta{
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
		Filename:      inferRemoteFilename(resp, sourceURL),
		SourceLabel:   cfg.From,
	}, resp.Body, nil
}

func openLocalSource(filePath string) (sourceMeta, io.ReadCloser, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return sourceMeta{}, nil, &shareError{
			Code:     "LOCAL_FILE_OPEN_FAILED",
			Message:  fmt.Sprintf("failed to open local file: %v", err),
			ExitCode: exitCodeSourceError,
		}
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return sourceMeta{}, nil, &shareError{
			Code:     "LOCAL_FILE_STAT_FAILED",
			Message:  fmt.Sprintf("failed to stat local file: %v", err),
			ExitCode: exitCodeSourceError,
		}
	}
	if info.IsDir() {
		file.Close()
		return sourceMeta{}, nil, &shareError{
			Code:     "INVALID_LOCAL_FILE",
			Message:  "local source path is a directory",
			ExitCode: exitCodeUsage,
		}
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	if contentType == "" {
		buf := make([]byte, 512)
		n, _ := file.Read(buf)
		contentType = http.DetectContentType(buf[:n])
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return sourceMeta{}, nil, &shareError{
				Code:     "LOCAL_FILE_SEEK_FAILED",
				Message:  fmt.Sprintf("failed to rewind local file: %v", err),
				ExitCode: exitCodeSourceError,
			}
		}
	}

	return sourceMeta{
		ContentLength: info.Size(),
		ContentType:   contentType,
		Filename:      info.Name(),
		SourceLabel:   filePath,
	}, file, nil
}

func openSourceWithRetry(ctx context.Context, client *http.Client, cfg shareConfig, sourceURL *url.URL) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, cfg.Method, sourceURL.String(), nil)
		if err != nil {
			return nil, &shareError{
				Code:     "SOURCE_REQUEST_BUILD_FAILED",
				Message:  fmt.Sprintf("failed to build source request: %v", err),
				ExitCode: exitCodeSourceError,
			}
		}

		for _, rawHeader := range cfg.Headers {
			parts := strings.SplitN(rawHeader, ":", 2)
			if len(parts) != 2 {
				return nil, &shareError{
					Code:     "INVALID_HEADER",
					Message:  fmt.Sprintf("invalid --header format: %q (use \"Key: Value\")", rawHeader),
					ExitCode: exitCodeUsage,
				}
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key == "" {
				return nil, &shareError{
					Code:     "INVALID_HEADER",
					Message:  "header name cannot be empty",
					ExitCode: exitCodeUsage,
				}
			}
			if isHopByHopHeader(key) {
				continue
			}
			req.Header.Add(key, value)
		}
		for _, rawCookie := range cfg.Cookies {
			parts := strings.SplitN(rawCookie, "=", 2)
			if len(parts) != 2 {
				return nil, &shareError{
					Code:     "INVALID_COOKIE",
					Message:  fmt.Sprintf("invalid --cookie format: %q (use \"k=v\")", rawCookie),
					ExitCode: exitCodeUsage,
				}
			}
			req.AddCookie(&http.Cookie{Name: strings.TrimSpace(parts[0]), Value: strings.TrimSpace(parts[1])})
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if cfg.Verbose {
				log.Printf("source attempt %d failed: %v", attempt, err)
			}
			if attempt < 3 {
				if sleepErr := sleepWithContext(ctx, time.Duration(attempt)*500*time.Millisecond); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			break
		}

		if resp.StatusCode >= 500 && attempt < 3 {
			io.CopyN(io.Discard, resp.Body, 1024)
			resp.Body.Close()
			if cfg.Verbose {
				log.Printf("source attempt %d got %d, retrying", attempt, resp.StatusCode)
			}
			if sleepErr := sleepWithContext(ctx, time.Duration(attempt)*500*time.Millisecond); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			bodySnippet := readBodySnippet(resp.Body)
			resp.Body.Close()
			return nil, &shareError{
				Code:     "SOURCE_HTTP_ERROR",
				Message:  fmt.Sprintf("source responded %d: %s", resp.StatusCode, bodySnippet),
				ExitCode: exitCodeSourceError,
			}
		}

		return resp, nil
	}

	return nil, &shareError{
		Code:     "SOURCE_CONNECT_FAILED",
		Message:  fmt.Sprintf("failed to fetch source after retries: %v", lastErr),
		ExitCode: exitCodeSourceError,
	}
}

func uploadFromSource(ctx context.Context, cfg shareConfig, client *http.Client, meta sourceMeta, sourceReader io.Reader) (string, error) {
	switch cfg.Provider {
	case "catbox":
		return uploadCatbox(ctx, cfg, client, meta, sourceReader)
	case "generic_put":
		return uploadGenericPut(ctx, cfg, client, meta, sourceReader)
	case "generic_multipart":
		return uploadGenericMultipart(ctx, cfg, client, meta, sourceReader)
	default:
		return "", &shareError{
			Code:     "INVALID_PROVIDER",
			Message:  "unsupported provider",
			ExitCode: exitCodeUsage,
		}
	}
}

func uploadCatbox(ctx context.Context, cfg shareConfig, client *http.Client, meta sourceMeta, body io.Reader) (string, error) {
	pipeReader, pipeWriter := io.Pipe()
	mpWriter := multipart.NewWriter(pipeWriter)

	filename := meta.Filename
	userHash := strings.TrimSpace(os.Getenv("KAI_CATBOX_USERHASH"))
	writeErr := make(chan error, 1)

	go func() {
		defer close(writeErr)
		if err := mpWriter.WriteField("reqtype", "fileupload"); err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if userHash != "" {
			if err := mpWriter.WriteField("userhash", userHash); err != nil {
				pipeWriter.CloseWithError(err)
				writeErr <- err
				return
			}
		}
		part, err := mpWriter.CreateFormFile("fileToUpload", filename)
		if err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if err := mpWriter.Close(); err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		writeErr <- pipeWriter.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://catbox.moe/user/api.php", pipeReader)
	if err != nil {
		pipeReader.Close()
		return "", &shareError{
			Code:     "UPLOAD_REQUEST_BUILD_FAILED",
			Message:  fmt.Sprintf("failed to build catbox upload request: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	req.Header.Set("Content-Type", mpWriter.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		pipeReader.Close()
		if writerErr := <-writeErr; writerErr != nil {
			if errors.Is(writerErr, errMaxSizeExceeded) {
				return "", writerErr
			}
		}
		return "", &shareError{
			Code:     "UPLOAD_FAILED",
			Message:  fmt.Sprintf("catbox upload failed: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	defer resp.Body.Close()

	if writerErr := <-writeErr; writerErr != nil {
		if errors.Is(writerErr, errMaxSizeExceeded) {
			return "", writerErr
		}
		return "", &shareError{
			Code:     "UPLOAD_STREAM_FAILED",
			Message:  fmt.Sprintf("failed while streaming catbox body: %v", writerErr),
			ExitCode: exitCodeUploadError,
		}
	}

	return parseUploadResponse(resp)
}

func uploadGenericPut(ctx context.Context, cfg shareConfig, client *http.Client, meta sourceMeta, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, cfg.To, body)
	if err != nil {
		return "", &shareError{
			Code:     "UPLOAD_REQUEST_BUILD_FAILED",
			Message:  fmt.Sprintf("failed to build upload request: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	if contentType := meta.ContentType; contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if meta.ContentLength >= 0 {
		req.ContentLength = meta.ContentLength
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", &shareError{
			Code:     "UPLOAD_FAILED",
			Message:  fmt.Sprintf("upload request failed: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	defer resp.Body.Close()

	return parseUploadResponse(resp)
}

func uploadGenericMultipart(ctx context.Context, cfg shareConfig, client *http.Client, meta sourceMeta, body io.Reader) (string, error) {
	pipeReader, pipeWriter := io.Pipe()
	mpWriter := multipart.NewWriter(pipeWriter)

	filename := meta.Filename
	writeErr := make(chan error, 1)

	go func() {
		defer close(writeErr)
		part, err := mpWriter.CreateFormFile("file", filename)
		if err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		if err := mpWriter.Close(); err != nil {
			pipeWriter.CloseWithError(err)
			writeErr <- err
			return
		}
		writeErr <- pipeWriter.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.To, pipeReader)
	if err != nil {
		pipeReader.Close()
		return "", &shareError{
			Code:     "UPLOAD_REQUEST_BUILD_FAILED",
			Message:  fmt.Sprintf("failed to build upload request: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	req.Header.Set("Content-Type", mpWriter.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		pipeReader.Close()
		if writerErr := <-writeErr; writerErr != nil {
			if errors.Is(writerErr, errMaxSizeExceeded) {
				return "", writerErr
			}
		}
		return "", &shareError{
			Code:     "UPLOAD_FAILED",
			Message:  fmt.Sprintf("upload request failed: %v", err),
			ExitCode: exitCodeUploadError,
		}
	}
	defer resp.Body.Close()

	if writerErr := <-writeErr; writerErr != nil {
		if errors.Is(writerErr, errMaxSizeExceeded) {
			return "", writerErr
		}
		return "", &shareError{
			Code:     "UPLOAD_STREAM_FAILED",
			Message:  fmt.Sprintf("failed while streaming multipart body: %v", writerErr),
			ExitCode: exitCodeUploadError,
		}
	}

	return parseUploadResponse(resp)
}

func parseUploadResponse(resp *http.Response) (string, error) {
	if location := strings.TrimSpace(resp.Header.Get("Location")); location != "" {
		if parsed, err := url.Parse(location); err == nil {
			if parsed.IsAbs() {
				return parsed.String(), nil
			}
			return resp.Request.URL.ResolveReference(parsed).String(), nil
		}
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	bodyText := strings.TrimSpace(string(body))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &shareError{
			Code:     "UPLOAD_HTTP_ERROR",
			Message:  fmt.Sprintf("upload responded %d: %s", resp.StatusCode, bodyText),
			ExitCode: exitCodeUploadError,
		}
	}

	if bodyText == "" {
		return "", &shareError{
			Code:     "NO_SHARE_URL",
			Message:  "upload succeeded but provider did not return a share URL",
			ExitCode: exitCodeUploadError,
		}
	}
	if strings.HasPrefix(bodyText, "http://") || strings.HasPrefix(bodyText, "https://") {
		return bodyText, nil
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err == nil {
		if urlText, ok := findURLInPayload(payload); ok {
			return urlText, nil
		}
	}

	return "", &shareError{
		Code:     "NO_SHARE_URL",
		Message:  fmt.Sprintf("upload succeeded but no share URL found in response: %s", bodyText),
		ExitCode: exitCodeUploadError,
	}
}

func findURLInPayload(value any) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			lowerKey := strings.ToLower(key)
			if lowerKey == "url" || lowerKey == "share_url" || lowerKey == "download_url" || lowerKey == "link" {
				if str, ok := child.(string); ok && (strings.HasPrefix(str, "http://") || strings.HasPrefix(str, "https://")) {
					return str, true
				}
			}
			if found, ok := findURLInPayload(child); ok {
				return found, true
			}
		}
	case []any:
		for _, item := range typed {
			if found, ok := findURLInPayload(item); ok {
				return found, true
			}
		}
	case string:
		if strings.HasPrefix(typed, "http://") || strings.HasPrefix(typed, "https://") {
			return typed, true
		}
	}
	return "", false
}

func newSafeTransport(cfg shareConfig, enforceAllowlist bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout: cfg.ConnectTimeout,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           safeDialContext(dialer, cfg, enforceAllowlist),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.ConnectTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

func safeDialContext(dialer *net.Dialer, cfg shareConfig, enforceAllowlist bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if enforceAllowlist {
			if err := validateHostAgainstAllowlist(host, cfg.AllowDomains); err != nil {
				return nil, err
			}
		}

		ipAddresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ipAddresses) == 0 {
			return nil, fmt.Errorf("no IPs found for host %q", host)
		}

		var lastErr error
		for _, ipAddr := range ipAddresses {
			if cfg.DenyPrivateIP && isPrivateIP(ipAddr.IP) {
				lastErr = fmt.Errorf("blocked private/link-local IP %s for host %s", ipAddr.IP.String(), host)
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
			if dialErr != nil {
				lastErr = dialErr
				continue
			}
			return conn, nil
		}

		if lastErr == nil {
			lastErr = fmt.Errorf("unable to connect to host %q", host)
		}
		return nil, lastErr
	}
}

func validateSourceURL(sourceURL *url.URL, cfg shareConfig) error {
	if sourceURL == nil {
		return &shareError{
			Code:     "INVALID_SOURCE_URL",
			Message:  "source URL is required",
			ExitCode: exitCodeUsage,
		}
	}
	if sourceURL.Scheme != "http" && sourceURL.Scheme != "https" {
		return &shareError{
			Code:     "INVALID_SOURCE_SCHEME",
			Message:  "source URL must use http or https",
			ExitCode: exitCodeUsage,
		}
	}
	if sourceURL.Hostname() == "" {
		return &shareError{
			Code:     "INVALID_SOURCE_URL",
			Message:  "source URL host is required",
			ExitCode: exitCodeUsage,
		}
	}
	if err := validateHostAgainstAllowlist(sourceURL.Hostname(), cfg.AllowDomains); err != nil {
		return &shareError{
			Code:     "SOURCE_DOMAIN_BLOCKED",
			Message:  err.Error(),
			ExitCode: exitCodeSafetyError,
		}
	}
	if cfg.DenyPrivateIP {
		if ip := net.ParseIP(sourceURL.Hostname()); ip != nil && isPrivateIP(ip) {
			return &shareError{
				Code:     "SOURCE_IP_BLOCKED",
				Message:  fmt.Sprintf("source IP %s is private/loopback/link-local and deny-private-ip is enabled", ip.String()),
				ExitCode: exitCodeSafetyError,
			}
		}
	}
	return nil
}

func validateHostAgainstAllowlist(host string, allowDomains []string) error {
	if len(allowDomains) == 0 {
		return nil
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, domain := range allowDomains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		domain = strings.TrimPrefix(domain, ".")
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return fmt.Errorf("host %q is not in allow-domain list", host)
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified()
}

func startProgressPrinter(ctx context.Context, cfg shareConfig, counter *atomic.Int64) func() {
	if !cfg.Progress {
		return func() {}
	}
	progressCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		var lastBytes int64
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
				current := counter.Load()
				delta := current - lastBytes
				lastBytes = current
				fmt.Fprintf(os.Stderr, "progress bytes=%d rate=%.2f MB/s\n", current, float64(delta)/(1024*1024))
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func classifyShareError(err error) *shareError {
	var se *shareError
	if errors.As(err, &se) {
		return se
	}
	return &shareError{
		Code:     "UNKNOWN_ERROR",
		Message:  err.Error(),
		ExitCode: exitCodeUploadError,
		Err:      err,
	}
}

func printShareSuccess(output string, res shareResult) {
	if output == "json" {
		payload := map[string]any{
			"ok":          true,
			"share_url":   res.ShareURL,
			"bytes":       res.Bytes,
			"duration_ms": res.DurationMS,
			"source":      res.Source,
			"provider":    res.Provider,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(payload)
		return
	}
	fmt.Printf("share_url=%s\n", res.ShareURL)
	fmt.Printf("bytes=%d\n", res.Bytes)
	fmt.Printf("duration=%s\n", time.Duration(res.DurationMS)*time.Millisecond)
}

func printShareError(output string, se *shareError) {
	if output == "json" {
		payload := map[string]any{
			"ok":      false,
			"code":    se.Code,
			"message": se.Error(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(payload)
		return
	}
	fmt.Fprintf(os.Stderr, "error (%s): %s\n", se.Code, se.Error())
}

func parseSize(raw string) (int64, error) {
	text := strings.TrimSpace(strings.ToUpper(raw))
	if text == "" {
		return 0, errors.New("empty size")
	}

	type unit struct {
		suffix     string
		multiplier int64
	}
	units := []unit{
		{suffix: "TB", multiplier: 1024 * 1024 * 1024 * 1024},
		{suffix: "GB", multiplier: 1024 * 1024 * 1024},
		{suffix: "MB", multiplier: 1024 * 1024},
		{suffix: "KB", multiplier: 1024},
		{suffix: "B", multiplier: 1},
	}

	for _, unit := range units {
		suffix := unit.suffix
		if strings.HasSuffix(text, suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(text, suffix))
			if number == "" {
				return 0, fmt.Errorf("invalid number in size %q", raw)
			}
			value, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return 0, err
			}
			if value < 0 {
				return 0, errors.New("size must be non-negative")
			}
			return value * unit.multiplier, nil
		}
	}

	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unknown size suffix in %q", raw)
	}
	if value < 0 {
		return 0, errors.New("size must be non-negative")
	}
	return value, nil
}

func inferRemoteFilename(sourceResp *http.Response, sourceURL *url.URL) string {
	if disposition := sourceResp.Header.Get("Content-Disposition"); disposition != "" {
		if _, params, err := mime.ParseMediaType(disposition); err == nil {
			if filename := strings.TrimSpace(params["filename"]); filename != "" {
				return filename
			}
		}
	}

	base := path.Base(sourceURL.Path)
	if base == "." || base == "/" || base == "" {
		return "shared.bin"
	}
	return base
}

func readBodySnippet(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 1024))
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		return "<empty>"
	}
	return snippet
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

type countingReader struct {
	r      io.Reader
	onRead func(int)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.onRead != nil {
		c.onRead(n)
	}
	return n, err
}

type maxSizeReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (m *maxSizeReader) Read(p []byte) (int, error) {
	if m.limit <= 0 {
		return 0, errMaxSizeExceeded
	}
	if m.read < m.limit {
		remaining := m.limit - m.read
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := m.r.Read(p)
		m.read += int64(n)
		return n, err
	}

	var probe [1]byte
	n, err := m.r.Read(probe[:])
	if n > 0 {
		return 0, errMaxSizeExceeded
	}
	if err == io.EOF {
		return 0, io.EOF
	}
	return 0, err
}
