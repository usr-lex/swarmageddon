// Package storetest gives tests a real PostgreSQL: $SWARM_TEST_POSTGRES_URL if
// set, otherwise a throwaway postgres:17 container started with docker. Tests
// skip when neither is available.
package storetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	once      sync.Once
	baseURL   string
	container string
	setupErr  error
	dbCounter int
	mu        sync.Mutex
)

// URL returns the URL of a fresh, empty database, or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if setupErr != nil {
		t.Skipf("no PostgreSQL for tests (%v); set SWARM_TEST_POSTGRES_URL or make `docker run postgres:17` work", setupErr)
	}
	mu.Lock()
	dbCounter++
	name := fmt.Sprintf("t%d_%d", os.Getpid(), dbCounter)
	mu.Unlock()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	i := strings.LastIndex(baseURL, "/")
	q := ""
	if j := strings.Index(baseURL[i:], "?"); j >= 0 {
		q = baseURL[i+j:]
	}
	return baseURL[:i+1] + name + q
}

// Stop removes the container, if one was started. Call it from TestMain.
func Stop() {
	if container != "" {
		_ = exec.Command("docker", "rm", "-f", container).Run()
	}
}

func start() {
	if u := os.Getenv("SWARM_TEST_POSTGRES_URL"); u != "" {
		baseURL = u
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		setupErr = err
		return
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "-e", "POSTGRES_PASSWORD=test",
		"-p", "127.0.0.1::5432", "postgres:17").CombinedOutput()
	if err != nil {
		setupErr = fmt.Errorf("docker run: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	container = strings.TrimSpace(string(out))
	port, err := exec.Command("docker", "port", container, "5432/tcp").Output()
	if err != nil {
		setupErr = fmt.Errorf("docker port: %v", err)
		return
	}
	hostPort := strings.TrimSpace(strings.Split(string(port), "\n")[0])
	baseURL = fmt.Sprintf("postgres://postgres:test@%s/postgres?sslmode=disable", hostPort)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, baseURL)
		cancel()
		if err == nil {
			var one int
			err = conn.QueryRow(context.Background(), "SELECT 1").Scan(&one)
			conn.Close(context.Background())
			if err == nil {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	setupErr = fmt.Errorf("postgres container did not become ready")
}
