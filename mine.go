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
	"runtime"
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
	challenge  [32]byte
	target     [32]byte
	block      uint64
	difficulty *big.Int // 2^256 / target, для расчёта ETA
}

// targetToDifficulty вычисляет ожидаемое число хэшей до решения
func targetToDifficulty(target [32]byte) *big.Int {
	t := new(big.Int).SetBytes(target[:])
	if t.Sign() == 0 {
		return new(big.Int)
	}
	max := new(big.Int).Lsh(big.NewInt(1), 256)
	return new(big.Int).Div(max, t)
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
	// CUDA-контекст привязан к OS-треду — фиксируем горутину на одном треде
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

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
	submits := 0
	startTime := time.Now()
	lastFound := time.Now()

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

			// Лог раз в ~100 батчей (~2 сек)
			if hashCount%(BATCH_SIZE*100) == 0 {
				elapsed := time.Since(startTime).Seconds()
				ghs := float64(hashCount) / elapsed / 1e9

				// ETA: сколько секунд в среднем до решения
				var eta string
				if current.difficulty != nil && ghs > 0 {
					diffF, _ := new(big.Float).SetInt(current.difficulty).Float64()
					etaSec := diffF / (ghs * 1e9)
					eta = fmt.Sprintf("~%.0fs", etaSec)
				} else {
					eta = "?"
				}

				// Блоков до смены эпохи
				blocksLeft := 100 - (current.block % 100)

				log("%.2f GH/s | block=%d (epoch -%d blk) | ETA=%s | solved=%d | since last=%.0fs",
					ghs,
					current.block,
					blocksLeft,
					eta,
					submits,
					time.Since(lastFound).Seconds(),
				)
			}

			if found {
				log("*** Nonce found: %s (after %.0fs)", nonce.String(), time.Since(lastFound).Seconds())
				if err := submitNonce(ctx, cfg, rpc, nonce); err != nil {
					log("Submit error: %v", err)
				} else {
					submits++
					lastFound = time.Now()
					log("*** mine(nonce) submitted | total solved: %d", submits)
					hashCount = 0
					startTime = time.Now()
				}
				time.Sleep(15 * time.Second)
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
	state.difficulty = targetToDifficulty(target)

	diff, _ := new(big.Float).SetInt(state.difficulty).Float64()
	log("Epoch updated | block=%d | challenge=%x | difficulty=%.2e",
		block, challenge[:8], diff)
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
