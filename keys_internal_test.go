package ark

import "testing"

// productionKDF is defaultKDF before TestMain lowers it.
var productionKDF = defaultKDF

func TestDefaultKDFIsRFC9106SecondOption(t *testing.T) {
	want := kdfParams{Time: 3, Memory: 64 * 1024, Threads: 4}
	if productionKDF != want {
		t.Errorf("defaultKDF = %+v, want %+v", productionKDF, want)
	}
}
