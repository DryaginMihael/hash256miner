CUDA_HOME ?= /usr/local/cuda
NVCC      := $(CUDA_HOME)/bin/nvcc
GO        := go

# RTX 4090 = sm_89 (Ada Lovelace)
GPU_ARCH  := sm_89

CUDA_SRCS := cuda/keccak_miner.cu
CUDA_LIB  := cuda/libkeccak_miner.a

.PHONY: all clean run

all: miner

# 1. Компилируем CUDA-ядро в статическую библиотеку
$(CUDA_LIB): $(CUDA_SRCS) cuda/keccak_miner.h
	$(NVCC) -O3 -arch=$(GPU_ARCH) \
		--generate-line-info \
		-Xcompiler -fPIC \
		-c cuda/keccak_miner.cu -o cuda/keccak_miner.o
	ar rcs $(CUDA_LIB) cuda/keccak_miner.o

# 2. Компилируем Go-бинарь (CGo подхватит libkeccak_miner.a через LDFLAGS в mine.go)
miner: $(CUDA_LIB) main.go mine.go rpc.go tx.go
	CGO_ENABLED=1 $(GO) build -o miner .

clean:
	rm -f cuda/*.o $(CUDA_LIB) miner

run: miner
	./miner

# Профилирование с Nsight (опционально)
profile: $(CUDA_LIB)
	CGO_ENABLED=1 $(GO) build -o miner .
	ncu --set full -o profile_report ./miner
