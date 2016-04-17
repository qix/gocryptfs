package fusefrontend

// Mkdir and Rmdir

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/fuse"

	"github.com/rfjakob/gocryptfs/internal/configfile"
	"github.com/rfjakob/gocryptfs/internal/cryptocore"
	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/toggledlog"
)

func (fs *FS) mkdirWithIv(cPath string, mode uint32) error {
	// Between the creation of the directory and the creation of gocryptfs.diriv
	// the directory is inconsistent. Take the lock to prevent other readers.
	fs.dirIVLock.Lock()
	// The new directory may take the place of an older one that is still in the cache
	fs.nameTransform.DirIVCache.Clear()
	defer fs.dirIVLock.Unlock()
	err := os.Mkdir(cPath, os.FileMode(mode))
	if err != nil {
		return err
	}
	// Create gocryptfs.diriv
	err = nametransform.WriteDirIV(cPath)
	if err != nil {
		err2 := syscall.Rmdir(cPath)
		if err2 != nil {
			toggledlog.Warn.Printf("mkdirWithIv: rollback failed: %v", err2)
		}
	}
	return err
}

func (fs *FS) Mkdir(newPath string, mode uint32, context *fuse.Context) (code fuse.Status) {
	if fs.isFiltered(newPath) {
		return fuse.EPERM
	}
	cPath, err := fs.getBackingPath(newPath)
	if err != nil {
		return fuse.ToStatus(err)
	}
	if !fs.args.DirIV {
		return fuse.ToStatus(os.Mkdir(cPath, os.FileMode(mode)))
	}
	// We need write and execute permissions to create gocryptfs.diriv
	origMode := mode
	mode = mode | 0300

	// Handle long file name
	cName := filepath.Base(cPath)
	if nametransform.IsLongContent(cName) {
		var dirfd *os.File
		dirfd, err = os.Open(filepath.Dir(cPath))
		if err != nil {
			return fuse.ToStatus(err)
		}
		defer dirfd.Close()

		// Create ".name"
		err = fs.nameTransform.WriteLongName(dirfd, cName, newPath)
		if err != nil {
			return fuse.ToStatus(err)
		}

		// Create directory
		err = fs.mkdirWithIv(cPath, mode)
		if err != nil {
			nametransform.DeleteLongName(dirfd, cName)
			return fuse.ToStatus(err)
		}
	} else {
		err = fs.mkdirWithIv(cPath, mode)
		if err != nil {
			return fuse.ToStatus(err)
		}
	}

	// Set permissions back to what the user wanted
	if origMode != mode {
		err = os.Chmod(cPath, os.FileMode(origMode))
		if err != nil {
			toggledlog.Warn.Printf("Mkdir: Chmod failed: %v", err)
		}
	}

	return fuse.OK
}

func (fs *FS) Rmdir(path string, context *fuse.Context) (code fuse.Status) {
	cPath, err := fs.getBackingPath(path)
	if err != nil {
		return fuse.ToStatus(err)
	}
	if !fs.args.DirIV {
		return fuse.ToStatus(syscall.Rmdir(cPath))
	}

	parentDir := filepath.Dir(cPath)
	parentDirFd, err := os.Open(parentDir)
	if err != nil {
		return fuse.ToStatus(err)
	}
	defer parentDirFd.Close()

	cName := filepath.Base(cPath)
	dirfdRaw, err := syscall.Openat(int(parentDirFd.Fd()), cName,
		syscall.O_RDONLY, 0)
	if err == syscall.EACCES {
		// We need permission to read and modify the directory
		toggledlog.Debug.Printf("Rmdir: handling EACCESS")
		// TODO use syscall.Fstatat once it is available in Go
		var fi os.FileInfo
		fi, err = os.Lstat(cPath)
		if err != nil {
			toggledlog.Debug.Printf("Rmdir: Stat: %v", err)
			return fuse.ToStatus(err)
		}
		origMode := fi.Mode()
		// TODO use syscall.Chmodat once it is available in Go
		err = os.Chmod(cPath, origMode|0700)
		if err != nil {
			toggledlog.Debug.Printf("Rmdir: Chmod failed: %v", err)
			return fuse.ToStatus(err)
		}
		// Retry open
		var st syscall.Stat_t
		syscall.Lstat(cPath, &st)
		dirfdRaw, err = syscall.Openat(int(parentDirFd.Fd()), cName,
			syscall.O_RDONLY, 0)
		// Undo the chmod if removing the directory failed
		defer func() {
			if code != fuse.OK {
				err := os.Chmod(cPath, origMode)
				if err != nil {
					toggledlog.Warn.Printf("Rmdir: Chmod rollback failed: %v", err)
				}
			}
		}()
	}
	if err != nil {
		toggledlog.Debug.Printf("Rmdir: Open: %v", err)
		return fuse.ToStatus(err)
	}
	dirfd := os.NewFile(uintptr(dirfdRaw), cName)
	defer dirfd.Close()

	children, err := dirfd.Readdirnames(10)
	if err != nil {
		toggledlog.Warn.Printf("Rmdir: Readdirnames: %v", err)
		return fuse.ToStatus(err)
	}
	// If the directory is not empty besides gocryptfs.diriv, do not even
	// attempt the dance around gocryptfs.diriv.
	if len(children) > 1 {
		return fuse.ToStatus(syscall.ENOTEMPTY)
	}

	// Move "gocryptfs.diriv" to the parent dir as "gocryptfs.diriv.rmdir.XYZ"
	tmpName := fmt.Sprintf("gocryptfs.diriv.rmdir.%d", cryptocore.RandUint64())
	toggledlog.Debug.Printf("Rmdir: Renaming %s to %s", nametransform.DirIVFilename, tmpName)
	// The directory is in an inconsistent state between rename and rmdir.
	// Protect against concurrent readers.
	fs.dirIVLock.Lock()
	defer fs.dirIVLock.Unlock()
	err = syscall.Renameat(int(dirfd.Fd()), nametransform.DirIVFilename,
		int(parentDirFd.Fd()), tmpName)
	if err != nil {
		toggledlog.Warn.Printf("Rmdir: Renaming %s to %s failed: %v",
			nametransform.DirIVFilename, tmpName, err)
		return fuse.ToStatus(err)
	}
	// Actual Rmdir
	// TODO Use syscall.Unlinkat with the AT_REMOVEDIR flag once it is available
	// in Go
	err = syscall.Rmdir(cPath)
	if err != nil {
		// This can happen if another file in the directory was created in the
		// meantime, undo the rename
		err2 := syscall.Renameat(int(parentDirFd.Fd()), tmpName,
			int(dirfd.Fd()), nametransform.DirIVFilename)
		if err != nil {
			toggledlog.Warn.Printf("Rmdir: Rename rollback failed: %v", err2)
		}
		return fuse.ToStatus(err)
	}
	// Delete "gocryptfs.diriv.rmdir.INODENUMBER"
	err = syscall.Unlinkat(int(parentDirFd.Fd()), tmpName)
	if err != nil {
		toggledlog.Warn.Printf("Rmdir: Could not clean up %s: %v", tmpName, err)
	}
	// Delete .name file
	if nametransform.IsLongContent(cName) {
		nametransform.DeleteLongName(parentDirFd, cName)
	}
	// The now-deleted directory may have been in the DirIV cache. Clear it.
	fs.nameTransform.DirIVCache.Clear()
	return fuse.OK
}

func (fs *FS) OpenDir(dirName string, context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	toggledlog.Debug.Printf("OpenDir(%s)", dirName)
	cDirName, err := fs.encryptPath(dirName)
	if err != nil {
		return nil, fuse.ToStatus(err)
	}
	// Read ciphertext directory
	cipherEntries, status := fs.FileSystem.OpenDir(cDirName, context)
	if cipherEntries == nil {
		return nil, status
	}
	// Get DirIV (stays nil if DirIV if off)
	var cachedIV []byte
	var cDirAbsPath string
	if fs.args.DirIV {
		// Read the DirIV once and use it for all later name decryptions
		cDirAbsPath = filepath.Join(fs.args.Cipherdir, cDirName)
		cachedIV, err = nametransform.ReadDirIV(cDirAbsPath)
		if err != nil {
			return nil, fuse.ToStatus(err)
		}
	}
	// Filter and decrypt filenames
	var plain []fuse.DirEntry
	for i := range cipherEntries {
		cName := cipherEntries[i].Name
		if dirName == "" && cName == configfile.ConfDefaultName {
			// silently ignore "gocryptfs.conf" in the top level dir
			continue
		}
		if fs.args.DirIV && cName == nametransform.DirIVFilename {
			// silently ignore "gocryptfs.diriv" everywhere if dirIV is enabled
			continue
		}

		if fs.args.PlaintextNames {
			plain = append(plain, cipherEntries[i])
			continue
		}

		if fs.args.LongNames {
			isLong := nametransform.NameType(cName)
			if isLong == nametransform.LongNameContent {
				cNameLong, err := nametransform.ReadLongName(filepath.Join(cDirAbsPath, cName))
				if err != nil {
					toggledlog.Warn.Printf("Could not read long name for file %s, skipping file", cName)
					continue
				}
				cName = cNameLong
			} else if isLong == nametransform.LongNameFilename {
				// ignore "gocryptfs.longname.*.name"
				continue
			}
		}
		name, err := fs.nameTransform.DecryptName(cName, cachedIV)
		if err != nil {
			toggledlog.Warn.Printf("Skipping invalid name '%s' in dir '%s': %s",
				cName, cDirName, err)
			continue
		}

		cipherEntries[i].Name = name
		plain = append(plain, cipherEntries[i])
	}
	return plain, status
}