package main

/*
#cgo LDFLAGS: -L${SRCDIR}/cuda -lkeccak_miner -lcudart -lstdc++
#include "cuda/keccak_miner.h"
*/
import "C"

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"
	"unsafe"
)

const (
	// Размер одного батча поиска нонсов: 2^26 ≈ 67M нонсов
	// RTX 4090 (~5 GH/s keccak) пройдёт это за ~13ms
	BATCH_SIZE = 1 << 26

	// Частота обновления challenge (проверяем блокчейн раз в N секунд)
	EPOCH_CHECK_INTERVAL = 30 * time.Second
)

type epochState struct {
	challenge [32]byte
	target    [32]byte
	block     uint64
}

func initGPU(deviceID int) error {
	if ret := C.cuda_init(C.int(deviceID)); ret != 0 {
		return fmt.Errorf("cuda_init failed: %d", ret)
	}
	return nil
}

func setChallenge(challenge, target [32]byte) {
	C.cuda_set_challenge(
		(*C.uint8_t)(unsafe.Pointer(&challenge[0])),
		(*C.uint8_t)(unsafe.Pointer(&target[0])),
	)
}

// gpuMine ищет нонс начиная с randomized стартовой точки.
// Возвращает нонс как *big.Int (uint64 в формате big-endian uint256).
func gpuMine(ctx context.Context, batchSize uint64) (nonce *big.Int, found bool) {
	// Случайная стартовая точка чтобы не конкурировать с другими майнерами
	var startBuf [8]byte
	rand.Read(startBuf[:])
	startNonce := binary.BigEndian.Uint64(startBuf[:])

	var outNonce C.uint64_t
	ret := C.cuda_mine(
		C.uint64_t(startNonce),
		C.uint64_t(batchSize),
		&outNonce,
	)
	if ret == 1 {
		n := new(big.Int).SetUint64(uint64(outNonce))
		return n, true
	}
	return nil, false
}

// miningLoop — главный цикл: следит за эпохой, запускает GPU, сабмитит нонс
func miningLoop(ctx context.Context, cfg *Config, rpc *rpcClient) error {
	if err := initGPU(cfg.GPUDevice); err != nil {
		return err
	}
	defer C.cuda_cleanup()

	var current epochState
	epochTicker := time.NewTicker(EPOCH_CHECK_INTERVAL)
	defer epochTicker.Stop()

	// Первоначальная загрузка challenge
	if err := updateEpoch(ctx, cfg, rpc, &current); err != nil {
		return fmt.Errorf("initial challenge fetch: %w", err)
	}
	setChallenge(current.challenge, current.target)

	hashCount := uint64(0)
	startTime := time.Now()

	log("Mining started | wallet=%s | device=%d", cfg.WalletAddress, cfg.GPUDevice)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-epochTicker.C:
			if err := maybeUpdateEpoch(ctx, cfg, rpc, &current); err != nil {
				log("epoch update error: %v", err)
			}

		default:
			nonce, found := gpuMine(ctx, BATCH_SIZE)
			hashCount += BATCH_SIZE

			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				ghs := float64(hashCount) / elapsed / 1e9
				log("Hashrate: %.2f GH/s | batches: %d", ghs, hashCount/BATCH_SIZE)
			}

			if found {
				log("Nonce found: %s", nonce.String())
				if err := submitNonce(ctx, cfg, rpc, nonce); err != nil {
					log("Submit error: %v", err)
				} else {
					log("mine(nonce) submitted successfully")
					hashCount = 0
					startTime = time.Now()
				}
				// После сабмита принудительно обновить challenge
				time.Sleep(15 * time.Second) // ждём включения в блок
				if err := updateEpoch(ctx, cfg, rpc, &current); err != nil {
					log("post-submit epoch update: %v", err)
				}
				setChallenge(current.challenge, current.target)
			}
		}
	}
}

func maybeUpdateEpoch(ctx context.Context, cfg *Config, rpc *rpcClient, cur *epochState) error {
	block, err := rpc.getBlockNumber(ctx)
	if err != nil {
		return err
	}
	if block <= cur.block+2 {
		return nil // ещё в той же эпохе
	}
	if err := updateEpoch(ctx, cfg, rpc, cur); err != nil {
		return err
	}
	setChallenge(cur.challenge, cur.target)
	return nil
}

func updateEpoch(ctx context.Context, cfg *Config, rpc *rpcClient, state *epochState) error {
	challenge, err := rpc.getChallenge(ctx, cfg.WalletAddress)
	if err != nil {
		return fmt.Errorf("getChallenge: %w", err)
	}
	target, err := rpc.getMiningTarget(ctx)
	if err != nil {
		return fmt.Errorf("getMiningTarget: %w", err)
	}
	block, err := rpc.getBlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("getBlockNumber: %w", err)
	}

	state.challenge = challenge
	state.target = target
	state.block = block

	log("Epoch updated | block=%d | challenge=%x | target=%x",
		block, challenge[:8], target[:8])
	return nil
}

func submitNonce(ctx context.Context, cfg *Config, rpc *rpcClient, nonce *big.Int) error {
	// Строим calldata
	calldata := rpc.buildMineCalldata(nonce)
	log("Submitting tx | nonce=%s | calldata=%s…", nonce.String(), calldata[:18])

	// Получаем gas price и nonce кошелька
	gasPriceResult, err := rpc.call(ctx, "eth_gasPrice")
	if err != nil {
		return err
	}
	txCountResult, err := rpc.call(ctx, "eth_getTransactionCount",
		cfg.WalletAddress, "pending")
	if err != nil {
		return err
	}

	var gasPriceHex, txCountHex string
	gasPriceResult.UnmarshalJSON([]byte(gasPriceResult))
	txCountResult.UnmarshalJSON([]byte(txCountResult))
	_ = gasPriceHex
	_ = txCountHex

	// Используем go-ethereum для подписи и отправки
	return sendSignedTx(ctx, cfg, calldata, nonce)
}
