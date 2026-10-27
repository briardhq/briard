// briard-setup is the Windows installer a stranger runs, a signed program in two builds that
// differ only in what they carry. The Store's (briard-store-setup.exe) carries the release's
// windows-amd64 arm IN ITSELF and runs that arm's install.ps1 over it; nothing is fetched while it
// runs, since the Store requires a standalone installer, and the guest image, which is data rather
// than a program Windows evaluates, is the installed agent's to fetch at its first start. The
// download page's (briard-setup.exe) carries only the release key, and runs stable's install.ps1
// once it verifies (web.go). Either way it is nothing more -- the install is the script's and the
// agent's, and this only unpacks and hands over -- so it adds no second implementation of the
// install's body.
//
// THE PAYLOAD IS A ZIP APPENDED TO THE PROGRAM, before its Authenticode signature: the release
// pipeline builds this program, appends the payload, then signs the whole file, so the signature
// covers the payload too. The program finds the payload's end at the signature's start (the PE
// security directory, a file offset) or at the end of the file when unsigned, and reads the zip
// back from there -- archive/zip finds an archive behind prepended bytes by itself.
package main

import (
	"archive/zip"
	"debug/pe"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// payload opens the archive appended to the program image f (size bytes long).
func payload(f io.ReaderAt, size int64) (*zip.Reader, error) {
	p, err := pe.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("reading the program's header: %w", err)
	}
	end := size
	if oh, ok := p.OptionalHeader.(*pe.OptionalHeader64); ok && len(oh.DataDirectory) > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
		if sd := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]; sd.VirtualAddress != 0 {
			end = int64(sd.VirtualAddress) // a file offset, unlike every other directory's
		}
	}
	z, err := zip.NewReader(io.NewSectionReader(f, 0, end), end)
	if err != nil {
		return nil, fmt.Errorf("this program carries no install (%w)", err)
	}
	return z, nil
}

// extract writes every entry of z under dir, refusing one that would land outside it.
func extract(z *zip.Reader, dir string) error {
	for _, e := range z.File {
		name := filepath.FromSlash(e.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("the payload's %q is not a path inside it", e.Name)
		}
		dst := filepath.Join(dir, name)
		if e.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := extractFile(e, dst); err != nil {
			return fmt.Errorf("unpacking %s: %w", e.Name, err)
		}
	}
	return nil
}

func extractFile(e *zip.File, dst string) error {
	in, err := e.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
