//go:build !linux

package importer

import (
	"context"
	"testing"
)

func TestNotOnThisPlatform(t *testing.T) {
	if _, err := Setup(context.Background(), nil, "", nil, nil, nil, nil); err == nil {
		t.Fatal("import set up outside Linux")
	}
	if (&Importer{}).Serve(context.Background(), nil) != nil {
		t.Fatal("stub Serve")
	}
}
