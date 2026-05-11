#pragma once
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Инициализация GPU (вызвать один раз при старте)
int  cuda_init(int device_id);

// Подготовить базовое состояние keccak с challenge (вызывать при смене эпохи)
// challenge_be: 32 байта big-endian (как из контракта)
// target_be:    32 байта big-endian (mining target)
void cuda_set_challenge(const uint8_t challenge_be[32], const uint8_t target_be[32]);

// Запустить один батч поиска нонса
// start_nonce:   с какого нонса начать (uint64, будет энкодирован как big-endian uint256)
// batch_size:    количество нонсов на батч (рекомендуется 1<<26 для RTX 4090)
// out_nonce:     найденный нонс (только если return == 1)
// Возвращает 1 если нашли, 0 если нет
int  cuda_mine(uint64_t start_nonce, uint64_t batch_size, uint64_t *out_nonce);

// Освободить ресурсы GPU
void cuda_cleanup(void);

#ifdef __cplusplus
}
#endif
