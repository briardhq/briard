package main

import (
	"archive/zip"
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// fakePE is the smallest image debug/pe reads: a DOS header pointing at the PE signature, the COFF
// header and a 64-bit optional header whose security directory starts at sigAt (0 for unsigned).
func fakePE(t *testing.T, sigAt uint32) []byte {
	t.Helper()
	var b bytes.Buffer
	dos := make([]byte, 0x40)
	dos[0], dos[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
	b.Write(dos)
	b.WriteString("PE\x00\x00")
	oh := pe.OptionalHeader64{Magic: 0x20b, NumberOfRvaAndSizes: 16}
	oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY] = pe.DataDirectory{VirtualAddress: sigAt, Size: 8}
	fh := pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_AMD64, SizeOfOptionalHeader: uint16(binary.Size(oh))}
	for _, v := range []any{fh, oh} {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The program, the arm appended, and -- once signed -- the signature after it: the payload reads
// back whole either way, and only what it carries is written.
func TestPayloadReadsBehindTheProgramAndBeforeItsSignature(t *testing.T) {
	arm := map[string]string{"install.ps1": "script", "briard-agent.exe": "agent", "qemu/bin/qemu-system-x86_64.exe": "qemu"}
	zipped := archive(t, arm)
	unsigned := append(fakePE(t, 0), zipped...)
	head := fakePE(t, 0)
	signed := append(fakePE(t, uint32(len(head)+len(zipped))), zipped...)
	// The signature's bytes are never the archive's, even when they happen to hold what reads as
	// a zip's end record -- here a decoy naming a directory that is not there.
	decoy := make([]byte, 22)
	copy(decoy, "PK\x05\x06")
	binary.LittleEndian.PutUint16(decoy[10:], 3)       // entries
	binary.LittleEndian.PutUint32(decoy[12:], 46)      // directory size
	binary.LittleEndian.PutUint32(decoy[16:], 0x10000) // directory offset
	signed = append(signed, append([]byte("WIN_CERTIFICATE bytes"), decoy...)...)
	for name, img := range map[string][]byte{"unsigned": unsigned, "signed": signed} {
		z, err := payload(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		dir := t.TempDir()
		if err := extract(z, dir); err != nil {
			t.Fatalf("%s: extract: %v", name, err)
		}
		for rel, body := range arm {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil || string(got) != body {
				t.Errorf("%s: %s = %q (%v), want %q", name, rel, got, err, body)
			}
		}
	}
	if _, err := payload(bytes.NewReader(fakePE(t, 0)), int64(len(fakePE(t, 0)))); err == nil {
		t.Error("a program with nothing appended read as carrying an install")
	}
}

func TestExtractRefusesAnEntryOutsideTheDirectory(t *testing.T) {
	for _, name := range []string{"../escape", "/abs", "a/../../escape"} {
		zipped := archive(t, map[string]string{name: "x"})
		z, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "in")
		if err := extract(z, dir); err == nil {
			t.Errorf("%q: extracted, want a refusal", name)
		}
	}
}
