#include <stdio.h>
#include <stdarg.h>

// A variadic function definition: va_start/va_arg/va_end walk the extra
// arguments. cg12 lowers these to its own va_list operations.
int isum(int n, ...) {
	va_list ap;
	va_start(ap, n);
	int s = 0;
	for (int i = 0; i < n; i++)
		s += va_arg(ap, int);
	va_end(ap);
	return s;
}

double davg(int n, ...) {
	va_list ap;
	va_start(ap, n);
	double s = 0;
	for (int i = 0; i < n; i++)
		s += va_arg(ap, double);
	va_end(ap);
	return s / n;
}

// Mixed argument types selected at run time, and forwarding a va_list to libc.
void tagged(int n, ...) {
	va_list ap;
	va_start(ap, n);
	for (int i = 0; i < n; i++) {
		int kind = va_arg(ap, int);
		if (kind == 0)
			printf(" int:%d", va_arg(ap, int));
		else if (kind == 1)
			printf(" dbl:%.1f", va_arg(ap, double));
		else
			printf(" str:%s", va_arg(ap, const char *));
	}
	va_end(ap);
	printf("\n");
}

int mylog(const char *fmt, ...) {
	va_list ap;
	va_start(ap, fmt);
	int r = vprintf(fmt, ap); // forward the list to libc
	va_end(ap);
	return r;
}

int main(void) {
	printf("isum=%d\n", isum(5, 1, 2, 3, 4, 5));
	printf("davg=%.2f\n", davg(3, 1.5, 2.5, 3.5));
	printf("tagged:");
	tagged(3, 0, 42, 1, 3.14, 2, "hi");
	mylog("fwd %s=%d %.2f\n", "x", 7, 2.5);
	return 0;
}
