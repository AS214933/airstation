package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/cheatsnake/airstation/internal/config"
	"github.com/cheatsnake/airstation/internal/http"
	"github.com/cheatsnake/airstation/internal/logger"
	"github.com/cheatsnake/airstation/internal/pkg/fs"
	"github.com/cheatsnake/airstation/internal/storage"
	"github.com/cheatsnake/airstation/internal/storage/sqlite"
)

func main() {
	conf := config.Load()

	fs.DeleteDirIfExists(conf.TmpDir)
	fs.MustDir(conf.TmpDir)
	fs.MustDir(conf.DBDir)

	stopSignal := make(chan os.Signal, 1)
	signal.Notify(stopSignal, os.Interrupt, syscall.SIGTERM)

	log := logger.New(conf.Debug)
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go fs.RunFileCleanup(cleanupCtx, conf.TmpDir, conf.TmpRetention, time.Hour, log.WithGroup("cleanup"))

	store, err := sqlite.New(path.Join(conf.DBDir, conf.DBFile), log.WithGroup("storage"))
	if err != nil {
		log.Error("Failed connect to database: " + err.Error())
		os.Exit(1)
	}

	httpServer := http.NewServer(store, conf, log)
	go httpServer.Run()
	_ = httpServer

	<-stopSignal
	shutdown(log, httpServer, store)
}

func shutdown(log *slog.Logger, srv *http.Server, store storage.Storage) {
	println()
	log.Info("Shutting down the app...")

	if err := srv.Shutdown(); err != nil {
		log.Error("Failed to shutdown HTTP server: " + err.Error())
	}

	err := store.Close()
	if err != nil {
		log.Error("Failed to close database connection: " + err.Error())
	}

	log.Info("App gracefully stopped")
}
