// Package media handles attachments: file naming and voice transcription.
package media

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var preferredExt = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"video/mp4":       ".mp4",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

// Ext returns a file extension for a MIME type.
func Ext(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	base = strings.TrimSpace(base)
	if ext, ok := preferredExt[base]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// InlineImage reports whether a harness can be shown the image directly.
func InlineImage(mimeType string) bool {
	switch strings.TrimSpace(strings.Split(mimeType, ";")[0]) {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// FileName builds a collision-free, shell-friendly name for an attachment.
func FileName(original, mimeType string, now time.Time, seq int) string {
	stem := now.UTC().Format("20060102-150405")
	if seq > 0 {
		stem += fmt.Sprintf("-%d", seq)
	}
	if original != "" {
		clean := strings.Trim(unsafeChars.ReplaceAllString(filepath.Base(original), "_"), "._")
		if clean != "" {
			if len(clean) > 80 {
				clean = clean[len(clean)-80:]
			}
			if filepath.Ext(clean) == "" {
				clean += Ext(mimeType)
			}
			return stem + "-" + clean
		}
	}
	return stem + Ext(mimeType)
}

// HumanSize formats a byte count.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// Whisper transcribes audio with whisper.cpp.
type Whisper struct {
	Binary  string // whisper-cli
	Model   string // ggml model file
	FFmpeg  string
	Threads int
}

// Available reports whether the binary and model are present.
func (w *Whisper) Available() bool {
	if w == nil || w.Binary == "" || w.Model == "" {
		return false
	}
	if _, err := exec.LookPath(w.Binary); err != nil {
		return false
	}
	if _, err := exec.LookPath(w.ffmpeg()); err != nil {
		return false
	}
	_, err := os.Stat(w.Model)
	return err == nil
}

func (w *Whisper) ffmpeg() string {
	if w.FFmpeg != "" {
		return w.FFmpeg
	}
	return "ffmpeg"
}

// Transcribe returns the text spoken in an audio file.
func (w *Whisper) Transcribe(ctx context.Context, path, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tmp, err := os.MkdirTemp("", "hermitator-whisper-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	wav := filepath.Join(tmp, "audio.wav")
	if err := run(ctx, w.ffmpeg(), "-nostdin", "-loglevel", "error", "-i", path, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav); err != nil {
		return "", err
	}
	threads := w.Threads
	if threads == 0 {
		threads = min(runtime.NumCPU(), 8)
	}
	base := filepath.Join(tmp, "out")
	args := []string{"-m", w.Model, "-f", wav, "-l", "auto", "-t", fmt.Sprint(threads), "-nt", "-np", "-otxt", "-of", base}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		// whisper.cpp's initial prompt: spellings it should prefer.
		args = append(args, "--prompt", prompt)
	}
	if err := run(ctx, w.Binary, args...); err != nil {
		return "", err
	}
	text, err := os.ReadFile(base + ".txt")
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(text)), " "), nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
