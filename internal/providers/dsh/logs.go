package dsh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/klauspost/compress/zstd"
)

const maxCompressedBytes = 32 << 20
const maxDecodedBytes = 128 << 20
const maxLineBytes = 16 << 20
const maxSessionFiles = 10000

var errSchema = errors.New("unsupported session format")
var errParse = errors.New("invalid session accounting")
var errBusy = errors.New("session changed during read")
var generationName = regexp.MustCompile(`^session(?:\.v([1-9][0-9]*))?\.jsonl(\.zstd)?$`)

// Diagnostics deliberately omit source paths, IDs and decoder error bodies.
type collectionError struct{ code string }

func (e *collectionError) Error() string               { return "dsh: " + e.code }
func (e *collectionError) CollectionErrorCode() string { return e.code }
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code := "parse_error"
	switch {
	case os.IsNotExist(err):
		code = "missing"
	case os.IsPermission(err):
		code = "permission"
	case errors.Is(err, errSchema):
		code = "unsupported_schema"
	case errors.Is(err, errBusy):
		code = "busy"
	}
	return &collectionError{code: code}
}

type generation struct {
	path      string
	version   int
	ambiguous bool
}

func sessionFiles(ctx context.Context, root string) ([]generation, error) {
	if root == "" {
		return nil, os.ErrNotExist
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errParse
	}
	selected := map[string]generation{}
	count := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		match := generationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil
		}
		count++
		if count > maxSessionFiles {
			return errParse
		}
		version := 0
		if match[1] != "" {
			version, err = strconv.Atoi(match[1])
			if err != nil {
				return errSchema
			}
		}
		parent := filepath.Dir(path)
		old, exists := selected[parent]
		if exists && version == old.version {
			old.ambiguous = true
			selected[parent] = old
		}
		if !exists || version > old.version {
			selected[parent] = generation{path: path, version: version}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	files := make([]generation, 0, len(selected))
	for _, file := range selected {
		if file.version != 4 || file.ambiguous {
			return nil, errSchema
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

// Read a complete snapshot. Torn/checksum-invalid frames and incomplete JSONL
// tails fail closed and are retried on a later poll; source logs are never repaired.
func readLog(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxCompressedBytes {
		return nil, errParse
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCompressedBytes+1))
	if err != nil || len(data) > maxCompressedBytes {
		return nil, errParse
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, errBusy
	}
	if !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errBusy
	}
	if strings.HasSuffix(path, ".zstd") {
		decoder, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxDecodedBytes), zstd.WithDecoderMaxWindow(maxDecodedBytes))
		if err != nil {
			return nil, errParse
		}
		defer decoder.Close()
		data, err = io.ReadAll(io.LimitReader(decoder, maxDecodedBytes+1))
		if err != nil || len(data) > maxDecodedBytes {
			return nil, errParse
		}
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errParse
	}
	return data, nil
}

func collect(ctx context.Context, root, account string) ([]shared.TelemetryEvent, error) {
	files, err := sessionFiles(ctx, root)
	if err != nil {
		return nil, err
	}
	var out []shared.TelemetryEvent
	seen := map[string]bool{}
	var reportedTotal, reportedReasoning int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := readLog(file.path)
		if err != nil {
			return nil, err
		}
		id, events, err := parseLog(ctx, data, account)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, errParse
		}
		seen[id] = true
		for _, event := range events {
			if reportedTotal > maxSafeInteger-*event.TotalTokens {
				return nil, errParse
			}
			reportedTotal += *event.TotalTokens
			if event.ReasoningTokens != nil {
				if reportedReasoning > maxSafeInteger-*event.ReasoningTokens {
					return nil, errParse
				}
				reportedReasoning += *event.ReasoningTokens
			}
		}
		out = append(out, events...)
		if len(out) > 250000 {
			return nil, errParse
		}
	}
	return out, nil
}

func logScanner(data []byte) *bufio.Scanner {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	return scanner
}
