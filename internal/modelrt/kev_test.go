package modelrt

import "testing"

func TestKevSourcePinIsComplete(t *testing.T) {
	if len(kevSource.SHA256) != 64 || kevSource.Bytes <= 0 {
		t.Error("incomplete kev source pin")
	}
	if kevSource.URL == "" && kevSource.Revision == "" {
		t.Error("the kev source pin names neither a URL nor a revision")
	}
}
