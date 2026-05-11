#include "keccak_miner.h"
#include <cuda_runtime.h>
#include <cstdio>
#include <cstring>

// ── keccak-f[1600] constants ─────────────────────────────────────────────────

__constant__ uint64_t RC[24] = {
    0x0000000000000001ULL, 0x0000000000008082ULL,
    0x800000000000808AULL, 0x8000000080008000ULL,
    0x000000000000808BULL, 0x0000000080000001ULL,
    0x8000000080008081ULL, 0x8000000000008009ULL,
    0x000000000000008AULL, 0x0000000000000088ULL,
    0x0000000080008009ULL, 0x000000008000000AULL,
    0x000000008000808BULL, 0x800000000000008BULL,
    0x8000000000008089ULL, 0x8000000000008003ULL,
    0x8000000000008002ULL, 0x8000000000000080ULL,
    0x000000000000800AULL, 0x800000008000000AULL,
    0x8000000080008081ULL, 0x8000000000008080ULL,
    0x0000000080000001ULL, 0x8000000080008008ULL,
};

// ── keccak-f[1600] permutation ───────────────────────────────────────────────

__device__ __forceinline__ uint64_t rotl64(uint64_t x, int n) {
    return (x << n) | (x >> (64 - n));
}

__device__ __forceinline__ uint64_t bswap64(uint64_t x) {
    // Swap bytes: big-endian ↔ little-endian
    return __byte_perm((uint32_t)(x >> 32), (uint32_t)x, 0x0123) |
           ((uint64_t)__byte_perm((uint32_t)(x >> 32), (uint32_t)x, 0x4567) << 32);
}

// Keccak rho rotation offsets for state index x + 5*y
__device__ __constant__ int RHO[25] = {
     0,  1, 62, 28, 27,
    36, 44,  6, 55, 20,
     3, 10, 43, 25, 39,
    41, 45, 15, 21,  8,
    18,  2, 61, 56, 14
};

__device__ void keccak_f1600(uint64_t st[25]) {
    #pragma unroll
    for (int round = 0; round < 24; round++) {
        // ── θ (Theta) ──
        uint64_t C0 = st[0] ^ st[5] ^ st[10] ^ st[15] ^ st[20];
        uint64_t C1 = st[1] ^ st[6] ^ st[11] ^ st[16] ^ st[21];
        uint64_t C2 = st[2] ^ st[7] ^ st[12] ^ st[17] ^ st[22];
        uint64_t C3 = st[3] ^ st[8] ^ st[13] ^ st[18] ^ st[23];
        uint64_t C4 = st[4] ^ st[9] ^ st[14] ^ st[19] ^ st[24];

        uint64_t D0 = C4 ^ rotl64(C1, 1);
        uint64_t D1 = C0 ^ rotl64(C2, 1);
        uint64_t D2 = C1 ^ rotl64(C3, 1);
        uint64_t D3 = C2 ^ rotl64(C4, 1);
        uint64_t D4 = C3 ^ rotl64(C0, 1);

        st[ 0] ^= D0; st[ 5] ^= D0; st[10] ^= D0; st[15] ^= D0; st[20] ^= D0;
        st[ 1] ^= D1; st[ 6] ^= D1; st[11] ^= D1; st[16] ^= D1; st[21] ^= D1;
        st[ 2] ^= D2; st[ 7] ^= D2; st[12] ^= D2; st[17] ^= D2; st[22] ^= D2;
        st[ 3] ^= D3; st[ 8] ^= D3; st[13] ^= D3; st[18] ^= D3; st[23] ^= D3;
        st[ 4] ^= D4; st[ 9] ^= D4; st[14] ^= D4; st[19] ^= D4; st[24] ^= D4;

        // ── ρ (Rho) ──
        #pragma unroll
        for (int i = 1; i < 25; i++)
            st[i] = rotl64(st[i], RHO[i]);

        // ── π (Pi): B[y + 5*((2x+3y)%5)] = A[x + 5*y] ──
        uint64_t B[25];
        #pragma unroll
        for (int y = 0; y < 5; y++)
            #pragma unroll
            for (int x = 0; x < 5; x++)
                B[y + 5 * ((2*x + 3*y) % 5)] = st[x + 5*y];

        // ── χ (Chi): A[x,y] = B[x,y] ^ (~B[x+1,y] & B[x+2,y]) ──
        #pragma unroll
        for (int y = 0; y < 5; y++) {
            int base = y * 5;
            st[base+0] = B[base+0] ^ (~B[base+1] & B[base+2]);
            st[base+1] = B[base+1] ^ (~B[base+2] & B[base+3]);
            st[base+2] = B[base+2] ^ (~B[base+3] & B[base+4]);
            st[base+3] = B[base+3] ^ (~B[base+4] & B[base+0]);
            st[base+4] = B[base+4] ^ (~B[base+0] & B[base+1]);
        }

        // ── ι (Iota) ──
        st[0] ^= RC[round];
    }
}

// ── GPU side storage ─────────────────────────────────────────────────────────

// Базовое состояние keccak с уже поглощённым challenge (25 × uint64)
// Нонс только в лейне [7], остальное фиксировано.
__constant__ uint64_t d_base_state[25];

// Target как 4 big-endian uint64 для сравнения
__constant__ uint64_t d_target_be[4];

// Результат поиска
__device__ int    d_found;
__device__ uint64_t d_result_nonce;

// ── Mining kernel ─────────────────────────────────────────────────────────────
//
// Вход: 64 байта = challenge(32) || nonce(32 big-endian uint256, uint64 в лейне 7)
// Проверка: keccak256(input) < target (big-endian сравнение)

__global__ void mine_kernel(uint64_t start_nonce) {
    if (d_found) return;

    uint64_t nonce = start_nonce + (uint64_t)blockIdx.x * blockDim.x + threadIdx.x;

    // Копируем base_state и XOR-им нонс в лейн [7]
    // Нонс = big-endian uint256, только нижние 8 байт (lanes 4-6 = 0, lane 7 = bswap(nonce))
    uint64_t st[25];
    #pragma unroll
    for (int i = 0; i < 25; i++) st[i] = d_base_state[i];
    st[7] ^= bswap64(nonce);

    keccak_f1600(st);

    // Сравниваем hash < target (big-endian)
    // hash_be[i] = bswap64(st[i])
    bool found = false;
    #pragma unroll
    for (int i = 0; i < 4; i++) {
        uint64_t h = bswap64(st[i]);
        if (h < d_target_be[i]) { found = true; break; }
        if (h > d_target_be[i]) { break; }
        // h == d_target_be[i] → продолжаем
    }

    if (found) {
        if (atomicCAS(&d_found, 0, 1) == 0)
            d_result_nonce = nonce;
    }
}

// ── Host-side helpers ─────────────────────────────────────────────────────────

static uint64_t load_be64(const uint8_t *p) {
    return ((uint64_t)p[0] << 56) | ((uint64_t)p[1] << 48) |
           ((uint64_t)p[2] << 40) | ((uint64_t)p[3] << 32) |
           ((uint64_t)p[4] << 24) | ((uint64_t)p[5] << 16) |
           ((uint64_t)p[6] <<  8) |  (uint64_t)p[7];
}

static uint64_t load_le64(const uint8_t *p) {
    return  (uint64_t)p[0]        | ((uint64_t)p[1] <<  8) |
           ((uint64_t)p[2] << 16) | ((uint64_t)p[3] << 24) |
           ((uint64_t)p[4] << 32) | ((uint64_t)p[5] << 40) |
           ((uint64_t)p[6] << 48) | ((uint64_t)p[7] << 56);
}

// ── Public API ────────────────────────────────────────────────────────────────

int cuda_init(int device_id) {
    cudaError_t err = cudaSetDevice(device_id);
    if (err != cudaSuccess) {
        fprintf(stderr, "[cuda] cudaSetDevice(%d): %s\n", device_id, cudaGetErrorString(err));
        return -1;
    }
    cudaDeviceProp prop;
    cudaGetDeviceProperties(&prop, device_id);
    fprintf(stderr, "[cuda] GPU: %s, SM count: %d, CC: %d.%d\n",
            prop.name, prop.multiProcessorCount, prop.major, prop.minor);
    return 0;
}

void cuda_set_challenge(const uint8_t challenge_be[32], const uint8_t target_be[32]) {
    // Строим base_state: поглощаем 64 байта = challenge(32) || zeros(32)
    // Нонс будет XOR-иться в лейн [7] отдельно в ядре.
    // Padding: rate=136, input=64, pad byte 64 → lane[8] ^= 0x01, byte 135 → lane[16] ^= 0x80..
    uint64_t base[25] = {};

    // Lanes 0-3: challenge (big-endian bytes читаем как little-endian uint64)
    for (int i = 0; i < 4; i++)
        base[i] = load_le64(challenge_be + i * 8);

    // Lanes 4-7: нонс-зависимые, базово = 0 (нонс XOR-ится в ядре для lane[7])

    // Padding keccak256: byte 64 = 0x01 → lane[8] bit 0
    base[8] = 0x01ULL;
    // byte 135 = 0x80 → lane[16] MSB (byte 7 of lane 16, bit 56..63)
    base[16] = 0x8000000000000000ULL;

    cudaMemcpyToSymbol(d_base_state, base, sizeof(base));

    // Target как big-endian uint64 для сравнения
    uint64_t tgt[4];
    for (int i = 0; i < 4; i++)
        tgt[i] = load_be64(target_be + i * 8);

    cudaMemcpyToSymbol(d_target_be, tgt, sizeof(tgt));

    // Сбросить флаг найденного нонса
    int zero = 0;
    cudaMemcpyToSymbol(d_found, &zero, sizeof(zero));

    fprintf(stderr, "[cuda] challenge set, target[0]=%016llx\n", (unsigned long long)tgt[0]);
}

int cuda_mine(uint64_t start_nonce, uint64_t batch_size, uint64_t *out_nonce) {
    // Сброс флага перед батчем
    int zero = 0;
    cudaMemcpyToSymbol(d_found, &zero, sizeof(zero));

    const int BLOCK_SIZE = 256;
    uint64_t total_threads = batch_size;
    uint64_t num_blocks = (total_threads + BLOCK_SIZE - 1) / BLOCK_SIZE;

    // RTX 4090: 128 SM × 2048 threads/SM = 262144 одновременных потоков
    // Запускаем несколькими волнами чтобы не блокировать надолго
    const uint64_t WAVE = 1ULL << 20; // 1M нонсов за вызов ядра
    uint64_t processed = 0;

    while (processed < total_threads) {
        uint64_t this_wave = (total_threads - processed < WAVE)
                           ? (total_threads - processed) : WAVE;
        uint64_t blocks = (this_wave + BLOCK_SIZE - 1) / BLOCK_SIZE;

        mine_kernel<<<(unsigned)blocks, BLOCK_SIZE>>>(start_nonce + processed);
        cudaError_t err = cudaDeviceSynchronize();
        if (err != cudaSuccess) {
            fprintf(stderr, "[cuda] kernel error: %s\n", cudaGetErrorString(err));
            return -1;
        }

        // Проверяем флаг
        int found = 0;
        cudaMemcpyFromSymbol(&found, d_found, sizeof(found));
        if (found) {
            uint64_t nonce = 0;
            cudaMemcpyFromSymbol(&nonce, d_result_nonce, sizeof(nonce));
            *out_nonce = nonce;
            return 1;
        }
        processed += this_wave;
    }
    return 0;
}

void cuda_cleanup(void) {
    cudaDeviceReset();
}
