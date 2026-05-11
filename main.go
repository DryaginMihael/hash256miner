package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	PrivateKey    string
	WalletAddress string
	RPCURL        string
	GPUDevice     int
}

func main() {
	godotenv.Load()

	cfg := &Config{
		PrivateKey:    mustEnv("MINING_PRIVATE_KEY"),
		WalletAddress: mustEnv("WALLET_ADDRESS"),
		RPCURL:        getEnv("ETH_RPC_URL", "https://hash256-production.up.railway.app/rpc"),
		GPUDevice:     envInt("GPU_DEVICE", 0),
	}

	log("hash256 GPU miner starting")
	log("Wallet : %s", cfg.WalletAddress)
	log("RPC    : %s", cfg.RPCURL)
	log("GPU    : device %d", cfg.GPUDevice)

	rpc := newRPCClient(cfg.RPCURL)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for {
		if err := miningLoop(ctx, cfg, rpc); err != nil {
			if ctx.Err() != nil {
				log("Shutting down")
				return
			}
			log("Mining loop error: %v — restarting in 10s", err)
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func log(format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "ERROR: %s not set\n", key)
		os.Exit(1)
	}
	return v
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}
