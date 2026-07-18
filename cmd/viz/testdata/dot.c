// Loop-invariant hoist. a*b does not depend on i, so the gcm column lifts it out
// of the loop body -- a transformation that is satisfying to watch because the
// code physically moves between columns.
long dot(long a, long b, long n) {
    long s = 0;
    for (long i = 0; i < n; i++)
        s += a*b + i;
    return s;
}
