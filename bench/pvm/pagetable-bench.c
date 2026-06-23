/*
 * Shadow-paging tax microbenchmark for the PVM spike (issue #40).
 *
 * Build static and run the SAME binary (a) natively on the host and (b) inside a
 * PVM guest (as init=/pagetable-bench), then compare. Native is a fair proxy for
 * the hardware-KVM-guest ceiling: with NPT/EPT, page faults and fork run close to
 * native, so the native-vs-PVM gap is approximately the PVM-vs-hardware-KVM gap.
 *
 *   gcc -O2 -static -o pagetable-bench pagetable-bench.c
 *   taskset -c 1 ./pagetable-bench                 # host native baseline
 *   # in guest: boot firecracker with init=/pagetable-bench, >=1536 MiB RAM
 *
 * Measured 2026-06-23 on a Hetzner CPX22 (AMD EPYC, 1 vCPU guest):
 *   cpu (no pagetable activity): host 0.412s  PVM 0.422s   ratio 1.02x (native)
 *   fault (page-fault storm):    host 0.882s  PVM 7.99s    ratio 9.1x
 *   fork  (20k fork+wait):       host 1.54s   PVM 14.35s   ratio 9.3x
 * Interpretation: ring-3 CPU work is native; pagetable-heavy work (the mitos
 * workload: forking sandboxes, pip installs) pays ~9x because every minor fault
 * and every fork is a VM exit to maintain shadow page tables. Single-run,
 * single-box, shared-cloud: directional, not a published benchmark.
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <sys/mman.h>
#include <sys/wait.h>

static double now(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec + t.tv_nsec / 1e9;
}

/* CPU-bound control: pure integer work, no pagetable activity. */
static void bench_cpu(void) {
    double t0 = now();
    volatile unsigned long x = 0;
    for (unsigned long i = 0; i < 500000000UL; i++)
        x += i * 1099511628211UL ^ (x >> 7);
    printf("RESULT cpu %.3f sink=%lu\n", now() - t0, (unsigned long)x);
}

/* Page-fault storm: minor faults are the shadow-paging worst case. */
static void bench_fault(void) {
    size_t REG = 256UL * 1024 * 1024, PS = 4096;
    int passes = 8;
    char *m = mmap(NULL, REG, PROT_READ | PROT_WRITE,
                   MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (m == MAP_FAILED) { perror("mmap"); return; }
    double t0 = now();
    unsigned long faults = 0;
    for (int p = 0; p < passes; p++) {
        for (size_t o = 0; o < REG; o += PS) { m[o] = (char)o; faults++; }
        madvise(m, REG, MADV_DONTNEED); /* drop pages so next pass re-faults */
    }
    printf("RESULT fault %.3f faults=%lu\n", now() - t0, faults);
    munmap(m, REG);
}

/* Fork storm: each fork rebuilds guest page tables (issue #40's named case). */
static void bench_fork(void) {
    int N = 20000;
    double t0 = now();
    for (int i = 0; i < N; i++) {
        pid_t pid = fork();
        if (pid == 0) { _exit(0); }
        else if (pid > 0) { int s; waitpid(pid, &s, 0); }
        else { perror("fork"); break; }
    }
    printf("RESULT fork %.3f n=%d\n", now() - t0, N);
}

int main(void) {
    printf("BENCH_START\n"); fflush(stdout);
    bench_cpu();   fflush(stdout);
    bench_fault(); fflush(stdout);
    bench_fork();  fflush(stdout);
    printf("BENCH_DONE\n"); fflush(stdout);
    /* As PID 1 in the guest, do not exit (kernel would panic). */
    if (getpid() == 1) { for (;;) pause(); }
    return 0;
}
