//go:build windows || linux || darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestCLIServerShutdownDrainsAndBoundsActiveRequests(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "idle"
		if active {
			name = "waiting_on_request_body"
		}
		t.Run(name, func(t *testing.T) {
			path := initializeCLI(t)
			ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
			defer cancel()
			child, address, output := launchCLIForShutdown(t, ctx, path)
			var blocked net.Conn
			if active {
				blocked = holdBodyRead(t, address)
				defer blocked.Close()
			}
			if err := signalCLIShutdown(child); err != nil {
				t.Fatal(err)
			}
			wait := child.Wait()
			if !active && wait != nil {
				t.Fatalf("idle listener did not shut down cleanly: %v\n%s", wait, output.String())
			}
			if active {
				var status *exec.ExitError
				if !errors.As(wait, &status) || status.ExitCode() != 1 || !strings.Contains(output.String(), "context deadline exceeded") {
					t.Fatalf("waiting request did not trigger bounded shutdown: %v\n%s", wait, output.String())
				}
			}
			app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
			if err != nil {
				t.Fatalf("shutdown left the evidence database unavailable: %v", err)
			}
			defer app.Close()
			if actor, err := app.Account("reviewer"); err != nil || !actor.CanOperate() {
				t.Fatalf("shutdown lost the initialized operator: %v", err)
			}
		})
	}
}

func launchCLIForShutdown(t *testing.T, ctx context.Context, path string) (*exec.Cmd, string, *bytes.Buffer) {
	t.Helper()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCLIExecutableFixture$", "--", "serve", "-db", path, "-listen", address)
	child.Env = append(os.Environ(), "REB_TEST_RUN_CLI=1")
	var output bytes.Buffer
	child.Stderr = &output
	configureCLIShutdown(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	client := &http.Client{Timeout: 600 * time.Millisecond}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/login")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && bytes.Contains(body, []byte("Sign in")) {
				return child, address, &output
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	t.Fatalf("application did not reach its actual login listener\n%s", output.String())
	return nil, "", nil
}

func holdBodyRead(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "POST /login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 128\r\nExpect: 100-continue\r\n\r\n", address)
	if err != nil {
		t.Fatal(err)
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || response != "HTTP/1.1 100 Continue\r\n" {
		t.Fatalf("server did not start reading the real request body: %q, %v", response, err)
	}
	return conn
}
