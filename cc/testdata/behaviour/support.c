/* Support the programs under test need from outside the compiler under test.
 *
 * __sync_synchronize is a GCC/Clang atomic builtin that cg12 does not implement,
 * so where those two inline a barrier cg12 emits a call to a symbol that does
 * not exist and the link fails. This is a real barrier, built by the host
 * compiler, linked into every arm of the test including the reference one -- so
 * it is the same in each and cannot flatter the compiler under test.
 *
 * It is a stand-in for a missing feature, not a workaround for a bug. Delete it
 * when cg12 grows the __sync_* builtins.
 */
void __sync_synchronize(void) { __atomic_thread_fence(__ATOMIC_SEQ_CST); }
