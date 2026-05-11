package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const (
	CONTRACT = "0xac7b5d06fa1e77d08aea40d46cb7c5923a87a0cc"

	// Найденные селекторы через eth_call
	SEL_GET_CHALLENGE = "0xf37381ad" // getChallenge(address) → bytes32
	SEL_MINING_TARGET = "0x5c062d6c" // miningTarget() → uint256
	SEL_EPOCH_BLOCKS  = "0x74c259c6" // EPOCH_BLOCKS() → uint256
	SEL_MINE          = "0x4d474898" // mine(uint256)
)

type rpcClient struct {
	endpoint string
	client   *http.Client
	id       int
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newRPCClient(endpoint string) *rpcClient {
	return &rpcClient{
		endpoint: endpoint,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *rpcClient) call(ctx context.Context, method string, params ...interface{}) (json.RawMessage, error) {
	r.id++
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params, ID: r.id}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", r.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func (r *rpcClient) ethCall(ctx context.Context, to, data string) (string, error) {
	result, err := r.call(ctx, "eth_call", map[string]string{"to": to, "data": data}, "latest")
	if err != nil {
		return "", err
	}
	var s string
	json.Unmarshal(result, &s)
	return s, nil
}

// getChallenge вызывает getChallenge(walletAddress) → bytes32
func (r *rpcClient) getChallenge(ctx context.Context, address string) ([32]byte, error) {
	addr := strings.TrimPrefix(strings.ToLower(address), "0x")
	data := SEL_GET_CHALLENGE + fmt.Sprintf("%064s", addr)

	hex_result, err := r.ethCall(ctx, CONTRACT, data)
	if err != nil {
		return [32]byte{}, err
	}
	return hexTo32(hex_result)
}

// getMiningTarget вызывает miningTarget() → uint256
func (r *rpcClient) getMiningTarget(ctx context.Context) ([32]byte, error) {
	hex_result, err := r.ethCall(ctx, CONTRACT, SEL_MINING_TARGET)
	if err != nil {
		return [32]byte{}, err
	}
	return hexTo32(hex_result)
}

// getBlockNumber возвращает текущий номер блока
func (r *rpcClient) getBlockNumber(ctx context.Context) (uint64, error) {
	result, err := r.call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	var s string
	json.Unmarshal(result, &s)
	n := new(big.Int)
	n.SetString(strings.TrimPrefix(s, "0x"), 16)
	return n.Uint64(), nil
}

// sendMine собирает и отправляет транзакцию mine(nonce)
func (r *rpcClient) buildMineCalldata(nonce *big.Int) string {
	// mine(uint256): selector + abi.encode(uint256)
	nonceHex := fmt.Sprintf("%064x", nonce)
	return SEL_MINE + nonceHex
}

func hexTo32(s string) ([32]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s) > 64 {
		s = s[len(s)-64:]
	}
	b, err := hex.DecodeString(fmt.Sprintf("%064s", s))
	if err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], b)
	return out, nil
}
