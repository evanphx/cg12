// Recursive Fibonacci. The showcase for cg12's bounded recursion unroller: the
// unroll column peels fib(n-1)+fib(n-2) into inlined copies, then the inliner
// and GVN work on the result.
long fib(long n) {
    if (n < 2) return n;
    return fib(n - 1) + fib(n - 2);
}
