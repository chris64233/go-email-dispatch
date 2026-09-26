// Command emaildispatch 启动抑制感知的批量邮件投递 HTTP 服务。
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	emaildispatch "github.com/chris64233/go-email-dispatch"
)

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	store := emaildispatch.NewMemoryStore()
	svc := emaildispatch.NewService(store)
	handler := emaildispatch.NewHTTPHandler(svc)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("emaildispatch listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
