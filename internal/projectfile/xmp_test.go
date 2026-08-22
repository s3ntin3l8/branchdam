package projectfile_test

import (
	"context"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

func TestXMPParser_Registration(t *testing.T) {
	parser, ok := projectfile.GetParser(".xmp")
	if !ok || parser == nil {
		t.Fatalf("expected XMPParser to be registered for .xmp")
	}

	parser2, ok := projectfile.GetParser("xmp")
	if !ok || parser2 == nil {
		t.Fatalf("expected XMPParser to be registered for xmp")
	}

	parser3, ok := projectfile.GetParser("photo.xmp")
	if !ok || parser3 == nil {
		t.Fatalf("expected XMPParser to be registered for photo.xmp")
	}
}

func TestXMPParser_Parse(t *testing.T) {
	parser := &projectfile.XMPParser{}

	t.Run("nil reader", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), nil)
		if err == nil {
			t.Fatal("expected error on nil reader, got nil")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		refs, err := parser.Parse(context.Background(), strings.NewReader(""))
		if err != nil {
			t.Fatalf("unexpected error on empty input: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("expected 0 refs, got %d", len(refs))
		}
	})

	t.Run("xmp with RawFileName attribute", func(t *testing.T) {
		content := `<x:xmpmeta xmlns:x="adobe:ns:meta/">
			<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
				<rdf:Description xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/"
					crs:RawFileName="DSC_1234.NEF"/>
			</rdf:RDF>
		</x:xmpmeta>`

		refs, err := parser.Parse(context.Background(), strings.NewReader(content))
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(refs))
		}
		if refs[0].RawPath != "DSC_1234.NEF" {
			t.Errorf("expected raw path DSC_1234.NEF, got %q", refs[0].RawPath)
		}
	})

	t.Run("xmp without explicit filename", func(t *testing.T) {
		content := `<x:xmpmeta xmlns:x="adobe:ns:meta/">
			<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
				<rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/">
					<dc:title><rdf:Alt><rdf:li xml:lang="x-default">My Photo</rdf:li></rdf:Alt></dc:title>
				</rdf:Description>
			</rdf:RDF>
		</x:xmpmeta>`

		refs, err := parser.Parse(context.Background(), strings.NewReader(content))
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(refs) != 0 {
			t.Errorf("expected 0 refs, got %d", len(refs))
		}
	})

	t.Run("malformed xml", func(t *testing.T) {
		content := `<x:xmpmeta><unclosed_tag></x:xmpmeta>`
		_, err := parser.Parse(context.Background(), strings.NewReader(content))
		if err == nil {
			t.Fatal("expected error on malformed xml, got nil")
		}
	})
}
