package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/KennethanCeyer/ptyx"
)

const version = "0.1.0"

func main() {
	// Parse flags.
	freq := flag.Float64("freq", 1.0, "Rainbow frequency/spread (0.1-10.0)")
	speed := flag.Float64("speed", 20.0, "Animation speed in Hz (0 = static)")
	redraw := flag.Float64("redraw", 0, "Static content redraw in Hz (0=off, 1-5 recommended). Caution: may flicker with TUI apps")
	showVer := flag.Bool("version", false, "Show version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: dpgk [options] <command> [args...]\n\n")
		fmt.Fprintf(os.Stderr, "Run a command and rainbow-color all its terminal output.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  dpgk ls -la\n")
		fmt.Fprintf(os.Stderr, "  dpgk --speed=30 vim main.go\n")
		fmt.Fprintf(os.Stderr, "  dpgk --freq=2.0 htop\n")
		fmt.Fprintf(os.Stderr, "  dpgk --redraw=2 vim  # animated static buffer (may flicker)\n")
	}
	flag.Parse()

	if *showVer {
		fmt.Printf("dpgk v%s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	// Resolve command.
	cmdName := flag.Arg(0)
	cmdArgs := flag.Args()[1:]

	fullPath, err := exec.LookPath(cmdName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dpgk: command not found: %s\n", cmdName)
		os.Exit(1)
	}

	// Run with rainbow PTY if terminal; otherwise exec directly.
	var exitCode int
	if isTerminal() {
		exitCode, err = runWithRainbow(fullPath, cmdArgs, *freq, *speed, *redraw)
	} else {
		exitCode, err = runDirect(fullPath, cmdArgs)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dpgk: %v\n", err)
	}
	os.Exit(exitCode)
}

// isTerminal checks if stdout is a terminal.
func isTerminal() bool {
	c, err := ptyx.NewConsole()
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// runDirect runs the command directly (passthrough, no rainbow).
func runDirect(path string, args []string) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// runWithRainbow spawns the command in a PTY and rainbow-colors its output.
// Returns the child's exit code and any error.
func runWithRainbow(path string, args []string, freq, speed, redrawHz float64) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get console.
	console, err := ptyx.NewConsole()
	if err != nil {
		return 1, fmt.Errorf("console: %w", err)
	}
	defer console.Close()

	// Enable VT processing (especially important on Windows).
	console.EnableVT()

	// Set terminal to raw mode so keystrokes pass through.
	rawState, err := console.MakeRaw()
	if err != nil {
		return 1, fmt.Errorf("make raw: %w", err)
	}
	// Restore terminal on any exit path.
	defer console.Restore(rawState)

	// Get initial terminal size.
	cols, rows := console.Size()

	// Spawn command in PTY.
	session, err := ptyx.Spawn(ctx, ptyx.SpawnOpts{
		Prog: path,
		Args: args,
		Cols: cols,
		Rows: rows,
		Env:  os.Environ(),
	})
	if err != nil {
		return 1, fmt.Errorf("spawn: %w", err)
	}
	defer session.Close()

	// Signal handling: forward signals to child process.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGINT:
				session.Kill() // same as SIGKILL
			default:
				session.Kill()
			}
		}
	}()

	// Handle terminal resize: forward to PTY.
	go func() {
		for range console.OnResize() {
			nc, nr := console.Size()
			_ = session.Resize(nc, nr)
		}
	}()

	// Set up rainbow transformer for PTY output.
	transform := NewRainbowTransformer(console.Out(), speed, freq*20, rows, cols, redrawHz)
	defer transform.Close()

	writerDone := make(chan error, 1)
	readerDone := make(chan error, 1)

	// Writer goroutine: stdin -> PTY.
	go func() {
		_, err := io.Copy(session.PtyWriter(), console.In())
		_ = session.CloseStdin()
		writerDone <- err
	}()

	// Reader goroutine: PTY -> rainbow transformer -> terminal.
	go func() {
		_, err := io.Copy(transform, session.PtyReader())
		readerDone <- err
	}()

	waitErr := session.Wait()

	// Close PTY input so writer goroutine's pending Write fails.
	// (If blocked on Read from console.In(), it stays alive until
	//  the next keystroke — negligible, not a leak.)
	session.CloseStdin()

	// Wait for reader goroutine to drain buffered PTY output.
	select {
	case <-readerDone:
	case <-time.After(3 * time.Second):
	}

	console.Out().Write([]byte{'\r'})

	session.Close()
	transform.Close()
	cancel()

	// Non-blocking drain of writer goroutine.
	select {
	case <-writerDone:
	default:
	}

	// Stop signal forwarding.
	signal.Stop(sigCh)
	close(sigCh)

	// Translate child exit code.
	if exitErr, ok := waitErr.(*ptyx.ExitError); ok {
		return exitErr.ExitCode, nil
	}
	if waitErr != nil {
		return 1, waitErr
	}
	return 0, nil
}
