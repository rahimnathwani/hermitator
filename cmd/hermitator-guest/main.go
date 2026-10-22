// Command hermitator-guest runs inside every agent sandbox.
//
//	hermitator-guest init                  PID 1: serve the job spool, reap orphans
//	hermitator-guest turn                  run one harness turn (request on stdin)
//	hermitator-guest send FILE [CAPTION]   queue a file for WhatsApp (also: whatsapp-send)
//
// The host mounts this binary read-only at /opt/hermitator/bin.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahimnathwani/hermitator/internal/harness"
	"github.com/rahimnathwani/hermitator/internal/spool"
	"github.com/rahimnathwani/hermitator/internal/turn"
)

var version = "dev"

func main() {
	if filepath.Base(os.Args[0]) == "whatsapp-send" {
		os.Exit(send(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "init":
		os.Exit(initMain())
	case "turn":
		os.Exit(turnMain())
	case "send":
		os.Exit(send(os.Args[2:]))
	case "schedule":
		os.Exit(schedule(os.Args[2:]))
	case "unschedule":
		os.Exit(unschedule(os.Args[2:]))
	case "tasks":
		os.Exit(tasks())
	case "ask":
		os.Exit(askPoll(os.Args[2:]))
	case "preview":
		os.Exit(preview(os.Args[2:]))
	case "hold":
		os.Exit(hold(os.Args[2:]))
	case "notify":
		os.Exit(notify(os.Args[2:]))
	case "check":
		os.Exit(check())
	case "recall":
		os.Exit(recall(os.Args[2:], os.Stdout))
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: hermitator-guest init | turn | send FILE [CAPTION] | schedule ... | unschedule ID | tasks | ask QUESTION OPTION... | preview PORT | hold COMMAND | notify COMMAND | recall WORDS... | version")
	os.Exit(2)
}

func initMain() int {
	// PID 1 must outlive a closed log stream; dying would take the whole
	// sandbox down with it.
	signal.Ignore(syscall.SIGPIPE)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv := &spool.Server{
		Root: harness.Getenv("HERMITATOR_SPOOL", turn.SpoolDir),
		Env:  os.Environ(),
		Log:  log,
	}
	log.Info("hermitator-guest init", "version", version, "pid", os.Getpid())
	if err := srv.Serve(ctx); err != nil {
		log.Error("spool", "err", err)
		return 1
	}
	return 0
}

func turnMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(e turn.Event) {
		mu.Lock()
		defer mu.Unlock()
		enc.Encode(e)
	}

	var req turn.Request
	data, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		emit(turn.Event{Type: turn.EventError, Text: "bad turn request: " + err.Error()})
		return 1
	}
	if err := harness.Run(ctx, &req, emit); err != nil {
		emit(turn.Event{Type: turn.EventError, Text: err.Error()})
		return 1
	}
	return 0
}

const maxSendBytes = 100 << 20 // WhatsApp's own limit for documents

func send(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: whatsapp-send FILE [CAPTION...]\n       whatsapp-send --text MESSAGE...\n\nSends FILE, or a text message, to the WhatsApp chat you are talking to.\nImages are sent as photos, anything else as a document. It works from\nbackground jobs too, after your turn has ended.")
		return 2
	}
	if args[0] == "--text" {
		return sendText(strings.Join(args[1:], " "))
	}
	src := args[0]
	caption := strings.Join(args[1:], " ")
	fi, err := os.Stat(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	if fi.IsDir() {
		fmt.Fprintln(os.Stderr, "whatsapp-send: is a directory; zip it first")
		return 1
	}
	if fi.Size() > maxSendBytes {
		fmt.Fprintf(os.Stderr, "whatsapp-send: %s is %d MB; the limit is %d MB\n", src, fi.Size()>>20, maxSendBytes>>20)
		return 1
	}
	outbox := filepath.Join(harness.Getenv("HERMITATOR_SHARED", turn.SharedDir), turn.OutboxDir)
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	name := fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(src))
	if err := copyFile(src, filepath.Join(outbox, name)); err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	meta, _ := json.Marshal(turn.OutboxMeta{File: name, Caption: caption, Turn: os.Getenv(turn.EnvTurn)})
	// The host acts on the sidecar, so it appears atomically and last.
	tmp := filepath.Join(outbox, "."+name+".meta.json")
	err = os.WriteFile(tmp, meta, 0o644)
	if err == nil {
		err = os.Rename(tmp, filepath.Join(outbox, name+".meta.json"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	fmt.Printf("Queued %s for WhatsApp; it will be delivered to the current chat.\n", filepath.Base(src))
	return 0
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sendText(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(os.Stderr, "usage: whatsapp-send --text MESSAGE...")
		return 2
	}
	outbox := filepath.Join(harness.Getenv("HERMITATOR_SHARED", turn.SharedDir), turn.OutboxDir)
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	name := fmt.Sprintf("%d-text", time.Now().UnixNano())
	meta, _ := json.Marshal(turn.OutboxMeta{Text: text, Turn: os.Getenv(turn.EnvTurn)})
	tmp := filepath.Join(outbox, "."+name+".meta.json")
	err := os.WriteFile(tmp, meta, 0o644)
	if err == nil {
		err = os.Rename(tmp, filepath.Join(outbox, name+".meta.json"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whatsapp-send:", err)
		return 1
	}
	fmt.Println("Queued the message for WhatsApp.")
	return 0
}
