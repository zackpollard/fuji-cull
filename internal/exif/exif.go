package exif

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// EnsurePath makes sure exiftool is reachable, adding Arch's vendor_perl dir if needed.
// Returns an error naming the package to install when exiftool is missing entirely.
func EnsurePath() error {
	if _, err := exec.LookPath("exiftool"); err == nil {
		return nil
	}
	if _, err := os.Stat("/usr/bin/vendor_perl/exiftool"); err == nil {
		os.Setenv("PATH", "/usr/bin/vendor_perl:"+os.Getenv("PATH"))
		return nil
	}
	return fmt.Errorf("exiftool not found (pacman -S perl-image-exiftool)")
}

// RestampMtime sets file mtime from EXIF capture time for JPEG and from QuickTime CreateDate for MOV/MP4.
// Recursively applied to the provided directory. Idempotent and safe to run repeatedly.
func RestampMtime(dir string) error {
	for _, args := range [][]string{
		{"-q", "-overwrite_original_in_place", "-ext", "JPG", "-FileModifyDate<DateTimeOriginal", dir},
		{"-q", "-overwrite_original_in_place", "-ext", "MOV", "-FileModifyDate<CreateDate", dir},
		{"-q", "-overwrite_original_in_place", "-ext", "MP4", "-FileModifyDate<CreateDate", dir},
		{"-q", "-overwrite_original_in_place", "-ext", "RAF", "-FileModifyDate<DateTimeOriginal", dir},
	} {
		c := exec.Command("exiftool", args...)
		var stderr bytes.Buffer
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			// exiftool exits 1 when no files match — not a real error.
			if stderr.Len() > 0 {
				return fmt.Errorf("exiftool %v: %w; stderr=%s", args, err, stderr.String())
			}
		}
	}
	return nil
}

// ExtractPreview writes the embedded JPEG preview of a RAF (or other raw) file to dst.
// Fuji bodies embed a full-resolution JPEG, and reading it over an MTP FUSE mount only
// transfers the bytes exiftool actually asks for.
func ExtractPreview(src, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	c := exec.Command("exiftool", "-b", "-PreviewImage", src)
	c.Stdout = out
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("exiftool preview %s: %w; stderr=%s", src, err, stderr.String())
	}
	st, err := out.Stat()
	if err != nil {
		return err
	}
	if st.Size() < 1024 {
		return fmt.Errorf("no embedded preview in %s (%d bytes extracted)", src, st.Size())
	}
	return nil
}
