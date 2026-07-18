// Collatz step count. Lights up the most passes: mem2reg turns the n/steps
// slots into loop phis, fold/strength-reduce rewrites 3*n and n/2 into shifts,
// and simplifycfg/dce clean up the ternary's diamond.
long collatz(long n) {
    long steps = 0;
    while (n != 1) {
        n = (n & 1) ? 3*n + 1 : n / 2;
        steps++;
    }
    return steps;
}
