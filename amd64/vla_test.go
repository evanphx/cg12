package amd64_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVLA exercises variable-length arrays on amd64 (run under qemu): 1-D and
// 2-D VLAs, a VLA parameter, sizeof, and a VLA in a loop.
func TestVLA(t *testing.T) {
	src := `
int sum1d(int n){ int a[n]; for(int i=0;i<n;i++)a[i]=i*i; int s=0; for(int i=0;i<n;i++)s+=a[i]; return s; }
int diag(int n,int m){ int a[n][m]; for(int i=0;i<n;i++)for(int j=0;j<m;j++)a[i][j]=i*m+j; return a[2][3]; }
int dot(int n,int x[n],int y[n]){ int s=0; for(int i=0;i<n;i++)s+=x[i]*y[i]; return s; }
int loops(int n){ int t=0; for(int k=0;k<64;k++){ int a[n]; for(int i=0;i<n;i++)a[i]=k+i; t=a[n-1]; } return t; }

int runtest(void){
	if(sum1d(5)!=30) return 1;
	if(sum1d(10)!=285) return 2;
	if(diag(4,5)!=13) return 3;
	int x[4]={1,2,3,4}, y[4]={5,6,7,8};
	if(dot(4,x,y)!=70) return 4;
	if(loops(8)!=70) return 5;      /* last k=63, a[7]=63+7=70 */
	int n=7; int b[n];
	if((int)sizeof(b)!=28) return 6;
	return 0;
}`
	require.Equal(t, 0, runC(t, src))
}
