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
| `0x07621eca` | `currentReward() → uint256` (100 tokens/block) |
| `0x74c259c6` | `EPOCH_BLOCKS() → uint256` (= 100) |

## CUDA kernel details

**Input to keccak256**: `abi.encodePacked(bytes32 challenge, uint256 nonce)` = 64 bytes

- Challenge (32 bytes) is absorbed into `base_state` once per epoch (`__constant__` memory)
- Nonce (uint64) is encoded as big-endian uint256: only lane `[7]` changes per thread → `state[7] ^= bswap64(nonce)`
- Padding: `state[8] ^= 0x01`, `state[16] ^= 0x8000000000000000` (keccak256, rate=136 bytes)
- Output comparison: `bswap64(state[0..3])` vs `target_be[0..3]` (big-endian)
- Winner written via `atomicCAS(&d_found, 0, 1)`

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
GPU_DEVICE=0
```

## Deploy to RunPod

```bash
cp .env.example .env && nano .env
./deploy.sh root@<pod-ip>   # rsync + remote build + launch
ssh root@<pod-ip> tail -f /opt/hash256-miner-go/miner.log
```

## Expected hashrates

| GPU | keccak256 |
|---|---|
| RTX 4090 | ~3–6 GH/s |
| RTX 4060 | ~800 MH/s–1.5 GH/s |
| Apple Silicon | not supported (no CUDA) |

## What NOT to do

- Do not add a browser/Puppeteer layer — the contract is fully accessible via direct RPC
- Do not store `.env` in git — it's in `.gitignore`
- Do not use `abi.encode` padding for the nonce in the kernel — the contract uses `abi.encodePacked` (no padding between fields, nonce is raw 32-byte big-endian)
