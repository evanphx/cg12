// Everything here is known at compile time, so the optimizer folds the whole
// function away: watch mem2reg promote the locals, fold and strength-reduce
// evaluate the arithmetic, and the final IR collapse to a single `ret 42` --
// the arm64 becomes a bare move of the constant.
long answer(void) {
    long a = 6;
    long b = 7;
    long product = a * b;         // 42
    long doubled = product * 2;   // 84
    long halved = doubled >> 1;   // 42
    return halved + a - b + 1;    // 42 + 6 - 7 + 1 = 42
}
