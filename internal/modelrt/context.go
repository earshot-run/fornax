package modelrt

// The context window a model was trained for, read from its GGUF header, so
// a served model is not silently pushed past what it knows.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/earshot-run/fornax/internal/catalog"
	"github.com/earshot-run/fornax/internal/gguf"
	"github.com/earshot-run/fornax/internal/paths"
)

// The context length the model's GGUF header declares
// ("<arch>.context_length"), or false when it is not a GGUF fornax can read.
func ModelContext(root string, spec *catalog.Spec) (int, bool) {
	if spec.Runtime != catalog.Llama {
		return 0, false
	}
	file, err := os.Open(paths.ModelFinal(root, spec))
	if err != nil {
		return 0, false
	}
	defer file.Close()
	_, _, kvs, err := gguf.Read(bufio.NewReader(file))
	if err != nil {
		return 0, false
	}
	for _, kv := range kvs {
		if strings.HasSuffix(kv.Key, ".context_length") {
			if n, err := strconv.Atoi(kv.Val); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// The context to serve with, lowered to the model's trained length when that
// is smaller than requested; note explains the change, or is "".
func contextFor(root string, spec *catalog.Spec, requested int) (int, string) {
	if trained, ok := ModelContext(root, spec); ok && trained < requested {
		return trained, fmt.Sprintf("%s was trained for %d tokens — serving at %d", spec.ID, trained, trained)
	}
	return requested, ""
}
