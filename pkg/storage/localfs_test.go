package storage

import (
	"context"
	"strings"
	"testing"
)

func TestLocalFS_List_SkipsMultipart(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewLocalFS([]string{dir})
	if err != nil { t.Fatalf("NewLocalFS: %v", err) }
	ctx := context.Background()

	if _, _, err := fs.Put(ctx, "bkt", "a.txt", strings.NewReader("a")); err != nil {
		t.Fatalf("Put a.txt: %v", err)
	}
	if _, _, err := fs.Put(ctx, "bkt", ".multipart/obj/up/part.1", strings.NewReader("x")); err != nil {
		t.Fatalf("Put multipart: %v", err)
	}

	objs, commonPrefixes, truncated, err := fs.List(ctx, "bkt", "", "", "", 100)
	if err != nil { t.Fatalf("List: %v", err) }
	if truncated { t.Fatalf("did not expect truncated") }
	if len(commonPrefixes) > 0 { t.Fatalf("unexpected prefixes") }
	if len(objs) != 1 { t.Fatalf("expected 1 object, got %d: %+v", len(objs), objs) }
	if objs[0].Key != "a.txt" { t.Fatalf("expected key a.txt, got %s", objs[0].Key) }
}

func TestLocalFS_IsBucketEmpty_IgnoresMultipart(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewLocalFS([]string{dir})
	if err != nil { t.Fatalf("NewLocalFS: %v", err) }
	ctx := context.Background()

	empty, err := fs.IsBucketEmpty(ctx, "bkt")
	if err != nil { t.Fatalf("IsBucketEmpty: %v", err) }
	if !empty { t.Fatalf("expected empty=true initially") }

	if _, _, err := fs.Put(ctx, "bkt", ".multipart/obj/up/part.1", strings.NewReader("x")); err != nil {
		t.Fatalf("Put multipart: %v", err)
	}
	empty, err = fs.IsBucketEmpty(ctx, "bkt")
	if err != nil { t.Fatalf("IsBucketEmpty: %v", err) }
	if !empty { t.Fatalf("expected empty=true with only multipart temp files") }

	if _, _, err := fs.Put(ctx, "bkt", "vis.txt", strings.NewReader("v")); err != nil {
		t.Fatalf("Put vis: %v", err)
	}
	empty, err = fs.IsBucketEmpty(ctx, "bkt")
	if err != nil { t.Fatalf("IsBucketEmpty: %v", err) }
	if empty { t.Fatalf("expected empty=false after visible object") }
}