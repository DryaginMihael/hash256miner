# hash256 GPU Miner

Go + CUDA miner for the hash256.org PoW contract on Ethereum mainnet. No browser required — communicates directly with the RPC proxy and the smart contract.

## Architecture

```
main.go      entry point, config, signal handling
mine.go      CGo bridge to CUDA, epoch loop, mining coordinator
rpc.go       raw JSON-RPC client (no external ETH lib dependency)
tx.go        EIP-1559 tx signing and broadcast via go-ethereum
cuda/
  keccak_miner.h   C API exposed to CGo
  keccak_miner.cu  CUDA kernel: keccak-f[1600] + mining search
Makefile     nvcc → libkeccak_miner.a, then go build
deploy.sh    rsync + remote build on RunPod in one command
```

### Related project

`~/Projects/hash256-dashboard` — standalone Go web dashboard (port 8080) showing remaining supply, difficulty, ETA at various hashrates. No dependencies, `go run main.go` to start.

## Protocol

- **Contract**: `0xac7b5d06fa1e77d08aea40d46cb7c5923a87a0cc` (Ethereum mainnet)
- **RPC proxy**: `https://hash256-production.up.railway.app/rpc` (load-balanced ETH nodes)
- **Challenge**: `getChallenge(walletAddress) → bytes32` — unique per address, changes each epoch
- **Mining**: brute-force `nonce` (uint64) until `keccak256(challenge ‖ nonce_as_uint256_be) < miningTarget`
- **Submit**: `mine(uint256 nonce)` on the contract
- **Epoch**: every 100 blocks (~20 min), challenge changes; miner polls every 30s

### Key contract selectors (discovered via eth_call)

| Selector | Function |
|---|---|
| `0xf37381ad` | `getChallenge(address) → bytes32` |
| `0x5c062d6c` | `miningTarget() → uint256` |
| `0x4d474898` | `mine(uint256 nonce)` |
| `0x07621eca` | `currentReward() → uint256` (100 HASH/block) |
| `0x74c259c6` | `EPOCH_BLOCKS() → uint256` (= 100) |
| `0x902d55a5` | `maxSupply() → uint256` (= 21,000,000 HASH) |
| `0x3271e471` | `totalMined() → uint256` (distributed to miners so far) |
| `0x18160ddd` | `totalSupply()` ERC20 |

### Token info

- **Name**: Hash, **Symbol**: HASH, **Decimals**: 18
- **Max supply**: 21,000,000 HASH
- **Reward**: 100 HASH per solved block
- **Epoch**: 100 blocks

## CUDA kernel details

**Input to keccak256**: `abi.encodePacked(bytes32 challenge, uint256 nonce)` = 64 bytes

- Challenge (32 bytes) is absorbed into `base_state` once per epoch (`__constant__` memory)
- Nonce (uint64) is encoded as big-endian uint256: only lane `[7]` changes per thread → `state[7] ^= bswap64(nonce)`
- Padding: `state[8] ^= 0x01`, `state[16] ^= 0x8000000000000000` (keccak256, rate=136 bytes)
- Output comparison: `bswap64(state[0..3])` vs `target_be[0..3]` (big-endian)
- Winner written via `atomicCAS(&d_found, 0, 1)`
- `g_device_id` stored globally; `cudaSetDevice(g_device_id)` called at start of every `cuda_mine()` call
- Go side uses `runtime.LockOSThread()` to keep CUDA context on one OS thread

**Tuning**:
- `BATCH_SIZE = 1<<26` (~67M nonces per kernel call, ~13ms on RTX 4090)
- Each call internally runs waves of `1<<20` to stay responsive to context cancellation
- `GPU_ARCH = sm_89` (Ada Lovelace: RTX 4060/4070/4080/4090); change to `sm_86` for Ampere (RTX 30xx)

## Build

```bash
# Requires: CUDA toolkit ≥12, Go ≥1.22, gcc
go mod tidy
make          # produces ./miner
make clean
```

## Config (.env)

```
MINING_PRIVATE_KEY=0x...   # dedicated mining wallet only
WALLET_ADDRESS=0x...
ETH_RPC_URL=https://hash256-production.up.railway.app/rpc
GPU_DEVICE=0               # 0-indexed, one process per GPU
```

## Multi-GPU

One process per GPU. `GPU_DEVICE` env var overrides the value in `.env` (godotenv does not overwrite existing env vars).

```bash
for GPU in $(seq 0 $(($(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)-1))); do
  GPU_DEVICE=$GPU nohup ./miner >> miner-gpu${GPU}.log 2>&1 &
done
```

Stop all: `pkill -f './miner'`

## One-command deploy on a fresh pod

Replace `0xТВОЙ_КЛЮЧ` and `0xТВОЙ_АДРЕС`, then paste as one block:

```bash
export PRIVATE_KEY=0xТВОЙ_КЛЮЧ && export WALLET=0xТВОЙ_АДРЕС && \
wget -q https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -O /tmp/go.tar.gz && \
tar -C /usr/local -xzf /tmp/go.tar.gz && \
export PATH=$PATH:/usr/local/go/bin:$(find /usr/local/cuda* -name nvcc 2>/dev/null | head -1 | xargs dirname) && \
export CUDA_HOME=$(find /usr/local -maxdepth 1 -name "cuda*" -type d 2>/dev/null | sort -V | tail -1) && \
git clone https://github.com/DryaginMihael/hash256miner.git /opt/hash256-miner-go && \
cd /opt/hash256-miner-go && \
printf "MINING_PRIVATE_KEY=$PRIVATE_KEY\nWALLET_ADDRESS=$WALLET\nETH_RPC_URL=https://hash256-production.up.railway.app/rpc\nGPU_DEVICE=0\n" > .env && \
go mod tidy && make && \
for GPU in $(seq 0 $(($(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l)-1))); do
  GPU_DEVICE=$GPU nohup ./miner >> miner-gpu${GPU}.log 2>&1 & echo "GPU $GPU PID: $!"
done && \
sleep 3 && nvidia-smi --query-gpu=index,utilization.gpu,power.draw --format=csv,noheader
```

## Expected hashrates

| GPU | keccak256 |
|---|---|
| RTX 4090 | ~3 GH/s |
| RTX 4060 | ~800 MH/s–1.5 GH/s |
| Apple Silicon | not supported (no CUDA) |

## What NOT to do

- Do not add a browser/Puppeteer layer — the contract is fully accessible via direct RPC
- Do not store `.env` in git — it's in `.gitignore`
- Do not use `abi.encode` padding for the nonce — the contract uses `abi.encodePacked` (nonce is raw 32-byte big-endian)
- Do not remove `runtime.LockOSThread()` — without it all processes fall back to GPU 0
- Do not remove `cudaSetDevice(g_device_id)` from `cuda_mine()` — needed because CGo can switch OS threads
