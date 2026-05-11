package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

func sendSignedTx(ctx context.Context, cfg *Config, calldata string, _ *big.Int) error {
	client, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return fmt.Errorf("ethclient dial: %w", err)
	}
	defer client.Close()

	privKey, err := parsePrivKey(cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	from := common.HexToAddress(cfg.WalletAddress)
	to := common.HexToAddress(CONTRACT)

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return fmt.Errorf("pending nonce: %w", err)
	}

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("chain id: %w", err)
	}

	// EIP-1559 fee estimate
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}

	baseFee := head.BaseFee
	maxPriorityFee := big.NewInt(1_500_000_000) // 1.5 gwei tip
	maxFee := new(big.Int).Add(
		new(big.Int).Mul(baseFee, big.NewInt(2)),
		maxPriorityFee,
	)

	data, _ := hex.DecodeString(strings.TrimPrefix(calldata, "0x"))

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		To:        &to,
		Value:     big.NewInt(0),
		Gas:       200_000,
		GasFeeCap: maxFee,
		GasTipCap: maxPriorityFee,
		Data:      data,
	})

	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, privKey)
	if err != nil {
		return fmt.Errorf("sign tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signed); err != nil {
		return fmt.Errorf("send tx: %w", err)
	}

	log("TX broadcast: %s", signed.Hash().Hex())
	return nil
}

func parsePrivKey(hexKey string) (*ecdsa.PrivateKey, error) {
	hexKey = strings.TrimPrefix(hexKey, "0x")
	return crypto.HexToECDSA(hexKey)
}
