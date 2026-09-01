// floor.cpp — R-1 floor micro-benchmark C++ counterpart to floor_test.go.
//
// Mirrors the IDENTICAL minimal per-watch loop over the IDENTICAL data shape
// (flat Watch arena + byte value array indexed by Blit), compiled with -O2 like
// minisat. Reports ns/watch so it can be compared directly to the Go floors.
//
// Build:  g++ -O2 -march=x86-64-v3 -o floor_cpp floor.cpp
// Run:    ./floor_cpp
#include <cstdint>
#include <cstdio>
#include <chrono>
#include <vector>
#include <random>

using Clock = std::chrono::steady_clock;

struct Watch {
    int32_t clauseIdx;
    uint32_t blit;
};
static_assert(sizeof(Watch) == 8, "Watch must be 8 bytes");

constexpr int numVars = 2000000;
constexpr int totalWatches = 8000000;

volatile uint64_t sink = 0;

double time_loop(const std::vector<Watch>& watches, const std::vector<uint8_t>& value) {
    int n = (int)watches.size();
    Watch const* w = watches.data();
    uint8_t const* v = value.data();
    uint64_t acc = 0;
    int reps = 20; // enough to stabilize
    auto t0 = Clock::now();
    for (int r = 0; r < reps; r++) {
        for (int i = 0; i < n; i++) {
            if (*(v + w[i].blit) != 0) continue;
            acc += (uint64_t)w[i].clauseIdx;
        }
    }
    auto t1 = Clock::now();
    sink = acc;
    double ns = std::chrono::duration<double, std::nano>(t1 - t0).count();
    return ns / ((double)n * reps);
}

int main() {
    std::mt19937 rng(1);
    std::uniform_int_distribution<int> distVar(0, numVars - 1);
    std::uniform_int_distribution<int> distB(0, 1);

    std::vector<Watch> watches(totalWatches);
    std::vector<uint8_t> value(numVars * 2, 0);
    for (int i = 0; i < totalWatches; i++) {
        int v = distVar(rng);
        watches[i] = Watch{(int32_t)i, (uint32_t)(v * 2 + distB(rng))};
    }
    for (int i = 0; i < (int)value.size(); i += 2) value[i] = 1;

    // warmup
    (void)time_loop(watches, value);
    // several timed samples
    double best = 1e18;
    for (int s = 0; s < 5; s++) {
        double ns = time_loop(watches, value);
        if (ns < best) best = ns;
    }
    std::printf("floor_cpp ns/watch=%.3f  watches/sec=%.0f\n",
                best, 1e9 / best);
    return 0;
}
