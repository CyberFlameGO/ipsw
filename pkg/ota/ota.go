package ota

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/apex/log"
	"github.com/blacktop/ipsw/internal/utils"
	"github.com/blacktop/ipsw/pkg/info"
	"github.com/blacktop/ipsw/pkg/ota/bom"
	"github.com/dustin/go-humanize"
	"golang.org/x/sys/execabs"

	"github.com/pkg/errors"
	// "github.com/ulikunitz/xz"
	"github.com/therootcompany/xz"
)

const (
	pbzxMagic     = 0x70627a78
	yaa1Header    = 0x31414159
	aa01Header    = 0x31304141
	hasMoreChunks = 0x800000
)

type pbzxHeader struct {
	Magic            uint32
	UncompressedSize uint64
}

type yaaHeader struct {
	Magic      uint32
	HeaderSize uint16
}

// headerMagic stores the magic bytes for the header
var headerMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
var yaaMagic = []byte{'Y', 'A', 'A', '1'}

// HeaderLen provides the length of the xz file header.
const HeaderLen = 12

type xzHeader struct {
	Flags uint64
	Size  uint64
}

type entry struct {
	Usually_0x210Or_0x110 uint32
	Usually_0x00_00       uint16 //_00_00;
	FileSize              uint32
	ModTime               uint64
	Whatever              uint16
	Usually_0x20          uint16
	NameLen               uint16
	Uid                   uint16
	Gid                   uint16
	Perms                 uint16
	//  char name[0];
	// Followed by file contents
}

type entryType byte

const (
	BlockSpecial     entryType = 'B'
	CharacterSpecial entryType = 'C'
	Directory        entryType = 'D'
	RegularFile      entryType = 'F'
	SymbolicLink     entryType = 'L'
	Metadata         entryType = 'M'
	Fifo             entryType = 'P'
	Socket           entryType = 'S'
)

// Entry is a YAA entry type
type Entry struct {
	Type entryType   // entry type
	Path string      // entry path
	Link string      // link path
	Uid  uint16      // user id
	Gid  uint16      // group id
	Mod  fs.FileMode // access mode
	Flag uint32      // BSD flags
	Mtm  time.Time   // modification time
	Size uint32      // file data size
	Aft  byte
	Afr  uint32
	Fli  uint32
}

func sortFileBySize(files []*zip.File) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].UncompressedSize64 > files[j].UncompressedSize64
	})
}

// List lists the files in the ota payloads
func List(otaZIP string) ([]os.FileInfo, error) {

	zr, err := zip.OpenReader(otaZIP)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open ota zip")
	}
	defer zr.Close()

	return parseBOM(&zr.Reader)
}

// RemoteList lists the files in a remote ota payloads
func RemoteList(zr *zip.Reader) ([]os.FileInfo, error) {
	return parseBOM(zr)
}

// TODO: maybe remove this as exec-ing is kinda gross
func NewXZReader(r io.Reader) (io.ReadCloser, error) {
	if _, err := execabs.LookPath("xz"); err != nil {
		xr, err := xz.NewReader(r, 0)
		if err != nil {
			return nil, err
		}
		return ioutil.NopCloser(xr), nil
	}

	rpipe, wpipe := io.Pipe()
	var errb bytes.Buffer
	cmd := execabs.Command("xz", "--decompress", "--stdout")
	cmd.Stdin = r
	cmd.Stdout = wpipe
	cmd.Stderr = &errb
	go func() {
		err := cmd.Run()
		if err != nil && errb.Len() != 0 {
			err = errors.New(strings.TrimRight(errb.String(), "\r\n"))
		}
		wpipe.CloseWithError(err)
	}()
	return rpipe, nil
}

func parseBOM(zr *zip.Reader) ([]os.FileInfo, error) {
	var validPostBOM = regexp.MustCompile(`post.bom$`)

	for _, f := range zr.File {
		if validPostBOM.MatchString(f.Name) {
			r, err := f.Open()
			if err != nil {
				return nil, errors.Wrapf(err, "failed to open file in zip: %s", f.Name)
			}
			bomData := make([]byte, f.UncompressedSize64)
			io.ReadFull(r, bomData)
			r.Close()
			return bom.Read(bytes.NewReader(bomData))
		}
	}

	return nil, fmt.Errorf("post.bom not found in zip")
}

func yaaDecodeHeader(r *bytes.Reader) (*Entry, error) {

	entry := &Entry{}
	field := make([]byte, 4)

	for {
		// Read Archive field
		err := binary.Read(r, binary.BigEndian, &field)

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		switch string(field[:3]) {
		case "TYP":
			switch field[3] {
			case '1':
				var etype byte
				if etype, err = r.ReadByte(); err != nil {
					return nil, err
				}
				entry.Type = entryType(etype)
			default:
				return nil, fmt.Errorf("found unknown TYP field: %s", string(field))
			}
		case "PAT":
			switch field[3] {
			case 'P':
				var pathLength uint16
				if err := binary.Read(r, binary.LittleEndian, &pathLength); err != nil {
					return nil, err
				}
				path := make([]byte, int(pathLength))
				if err := binary.Read(r, binary.LittleEndian, &path); err != nil {
					return nil, err
				}
				entry.Path = string(path)
			default:
				return nil, fmt.Errorf("found unknown PAT field: %s", string(field))
			}
		case "LNK":
			switch field[3] {
			case 'P':
				var pathLength uint16
				if err := binary.Read(r, binary.LittleEndian, &pathLength); err != nil {
					return nil, err
				}
				path := make([]byte, int(pathLength))
				if err := binary.Read(r, binary.LittleEndian, &path); err != nil {
					return nil, err
				}
				entry.Link = string(path)
			default:
				return nil, fmt.Errorf("found unknown LNK field: %s", string(field))
			}
		case "UID":
			switch field[3] {
			case '1':
				var dat byte
				if err := binary.Read(r, binary.LittleEndian, &dat); err != nil {
					return nil, err
				}
				entry.Uid = uint16(dat)
			case '2':
				if err := binary.Read(r, binary.LittleEndian, &entry.Uid); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("found unknown UID field: %s", string(field))
			}
		case "GID":
			switch field[3] {
			case '1':
				var dat byte
				if err := binary.Read(r, binary.LittleEndian, &dat); err != nil {
					return nil, err
				}
				entry.Gid = uint16(dat)
			case '2':
				if err := binary.Read(r, binary.LittleEndian, &entry.Gid); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("found unknown UID field: %s", string(field))
			}
		case "MOD":
			switch field[3] {
			case '2':
				var mod uint16
				if err := binary.Read(r, binary.LittleEndian, &mod); err != nil {
					return nil, err
				}
				entry.Mod = fs.FileMode(mod)
			default:
				return nil, fmt.Errorf("found unknown MOD field: %s", string(field))
			}
		case "FLG":
			switch field[3] {
			case '1':
				flag, err := r.ReadByte()
				if err != nil {
					return nil, err
				}
				entry.Flag = uint32(flag)
			case '4':
				if err := binary.Read(r, binary.LittleEndian, &entry.Flag); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("found unknown FLG field: %s", string(field))
			}
		case "MTM":
			switch field[3] {
			case 'T':
				var secs int64
				var nsecs int32
				if err := binary.Read(r, binary.LittleEndian, &secs); err != nil {
					return nil, err
				}
				if err := binary.Read(r, binary.LittleEndian, &nsecs); err != nil {
					return nil, err
				}
				entry.Mtm = time.Unix(secs, int64(nsecs))
			case 'S':
				var secs int64
				if err := binary.Read(r, binary.LittleEndian, &secs); err != nil {
					return nil, err
				}
				entry.Mtm = time.Unix(secs, 0)
			default:
				return nil, fmt.Errorf("found unknown MTM field: %s", string(field))
			}
		case "DAT":
			switch field[3] {
			case 'B':
				if err := binary.Read(r, binary.LittleEndian, &entry.Size); err != nil {
					return nil, err
				}
			case 'A':
				var dat uint16
				if err := binary.Read(r, binary.LittleEndian, &dat); err != nil {
					return nil, err
				}
				entry.Size = uint32(dat)
			default:
				return nil, fmt.Errorf("found unknown DAT field: %s", string(field))
			}
		case "AFT":
			switch field[3] {
			case '1':
				entry.Aft, err = r.ReadByte()
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("found unknown AFT field: %s", string(field))
			}
		case "AFR":
			switch field[3] {
			case '4':
				if err := binary.Read(r, binary.LittleEndian, &entry.Afr); err != nil {
					return nil, err
				}
			case '2':
				var dat uint16
				if err := binary.Read(r, binary.LittleEndian, &dat); err != nil {
					return nil, err
				}
				entry.Afr = uint32(dat)
			case '1':
				var dat byte
				if err := binary.Read(r, binary.LittleEndian, &dat); err != nil {
					return nil, err
				}
				entry.Afr = uint32(dat)
			default:
				return nil, fmt.Errorf("found unknown AFR field: %s", string(field))
			}
		case "FLI":
			switch field[3] {
			case '4':
				if err := binary.Read(r, binary.LittleEndian, &entry.Fli); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("found unknown FLI field: %s", string(field))
			}
		default:
			return nil, fmt.Errorf("found unknown YAA header field: %s", string(field))
		}
	}

	return entry, nil
}

// Extract extracts and decompresses OTA payload files
func Extract(otaZIP, extractPattern string) error {

	zr, err := zip.OpenReader(otaZIP)
	if err != nil {
		return errors.Wrap(err, "failed to open ota zip")
	}
	defer zr.Close()

	return parsePayload(&zr.Reader, extractPattern)
}

func getFolder(zr *zip.Reader) (string, error) {
	i, err := info.ParseZipFiles(zr.File)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse plists in remote zip")
	}

	folders := i.GetFolders()
	if len(folders) == 0 {
		return "", fmt.Errorf("failed to get folder")
	}

	return folders[0], nil
}

// RemoteExtract extracts and decompresses remote OTA payload files
func RemoteExtract(zr *zip.Reader, extractPattern string) error {

	var validPayload = regexp.MustCompile(`payload.0\d+$`)

	folder, err := getFolder(zr)
	if err != nil {
		return err
	}

	sortFileBySize(zr.File)

	for _, f := range zr.File {
		if validPayload.MatchString(f.Name) {
			utils.Indent(log.WithFields(log.Fields{
				"filename": f.Name,
				"size":     humanize.Bytes(f.UncompressedSize64),
			}).Debug, 2)("Processing OTA payload")
			found, err := Parse(f, folder, extractPattern)
			if err != nil {
				log.Error(err.Error())
			}
			if found {
				return nil
			}
		}
	}

	return fmt.Errorf("dyld_shared_cache not found")
}

func parsePayload(zr *zip.Reader, extractPattern string) error {
	var validPayload = regexp.MustCompile(`payload.0\d+$`)

	folder, err := getFolder(zr)
	if err != nil {
		return err
	}

	sortFileBySize(zr.File)

	for _, f := range zr.File {
		if validPayload.MatchString(f.Name) {
			utils.Indent(log.WithFields(log.Fields{
				"filename": f.Name,
				"size":     humanize.Bytes(f.UncompressedSize64),
			}).Debug, 2)("Processing OTA payload")
			found, err := Parse(f, folder, extractPattern)
			if err != nil {
				log.Error(err.Error())
			}
			if found {
				return nil
			}
		}
	}

	return fmt.Errorf("no files matched: %s", extractPattern)
}

// Parse parses a ota payload file inside the zip
func Parse(payload *zip.File, folder, extractPattern string) (bool, error) {

	pData := make([]byte, payload.UncompressedSize64)

	rc, err := payload.Open()
	if err != nil {
		return false, errors.Wrapf(err, "failed to open file in zip: %s", payload.Name)
	}

	io.ReadFull(rc, pData)
	rc.Close()

	pr := bytes.NewReader(pData)

	var pbzx pbzxHeader
	if err := binary.Read(pr, binary.BigEndian, &pbzx); err != nil {
		return false, err
	}

	if pbzx.Magic != pbzxMagic {
		return false, errors.New("src not a pbzx stream")
	}

	xzBuf := new(bytes.Buffer)

	for {
		var xzTag xzHeader
		if err := binary.Read(pr, binary.BigEndian, &xzTag); err != nil {
			return false, err
		}

		xzChunkBuf := make([]byte, xzTag.Size)
		if err := binary.Read(pr, binary.BigEndian, &xzChunkBuf); err != nil {
			return false, err
		}

		// xr, err := xz.NewReader(bytes.NewReader(xzChunkBuf))
		// xr, err := xz.NewReader(bytes.NewReader(xzChunkBuf), 0)
		xr, err := NewXZReader(bytes.NewReader(xzChunkBuf))
		if err != nil {
			return false, err
		}
		defer xr.Close()

		io.Copy(xzBuf, xr)

		if (xzTag.Flags & hasMoreChunks) == 0 {
			break
		}
	}

	rr := bytes.NewReader(xzBuf.Bytes())

	var magic uint32
	var headerSize uint16

	for {
		var ent *Entry

		err := binary.Read(rr, binary.LittleEndian, &magic)

		if err == io.EOF {
			break
		}

		if err != nil {
			return false, err
		}

		if magic == yaa1Header || magic == aa01Header { // NEW iOS 14.x OTA payload format
			if err := binary.Read(rr, binary.LittleEndian, &headerSize); err != nil {
				return false, err
			}
			header := make([]byte, headerSize-uint16(binary.Size(magic))-uint16(binary.Size(headerSize)))
			if err := binary.Read(rr, binary.LittleEndian, &header); err != nil {
				return false, err
			}

			ent, err = yaaDecodeHeader(bytes.NewReader(header))
			if err != nil {
				// dump header if in Verbose mode
				utils.Indent(log.Debug, 2)(hex.Dump(header))
				return false, err
			}

		} else { // pre iOS14.x OTA file
			var e entry
			if err := binary.Read(rr, binary.BigEndian, &e); err != nil {
				if err == io.EOF {
					break
				}
				return false, err
			}

			// 0x10030000 seem to be framworks and other important platform binaries (or symlinks?)
			if e.Usually_0x210Or_0x110 != 0x10010000 && e.Usually_0x210Or_0x110 != 0x10020000 && e.Usually_0x210Or_0x110 != 0x10030000 {
				// if e.Usually_0x210Or_0x110 != 0 {
				// 	log.Warnf("found unknown entry flag: 0x%x", e.Usually_0x210Or_0x110)
				// }
				break
			}

			fileName := make([]byte, e.NameLen)
			if err := binary.Read(rr, binary.BigEndian, &fileName); err != nil {
				if err == io.EOF {
					break
				}
				return false, err
			}

			// if e.Usually_0x20 != 0x20 {
			// 	fmt.Printf("%s: %#v\n", fileName, e)
			// }

			// if e.Usually_0x00_00 != 0 {
			// 	fmt.Printf("%s: %#v\n", fileName, e)
			// }

			if e.Usually_0x210Or_0x110 == 0x10030000 {
				fmt.Printf("%s (%s): %#v\n", fileName, os.FileMode(e.Perms), e)
			}

			ent.Mod = os.FileMode(e.Perms)
			ent.Path = string(fileName)
			ent.Size = e.FileSize
		}

		if len(extractPattern) > 0 {
			if strings.Contains(strings.ToLower(string(ent.Path)), strings.ToLower(extractPattern)) {
				fileBytes := make([]byte, ent.Size)
				if err := binary.Read(rr, binary.LittleEndian, &fileBytes); err != nil {
					if err == io.EOF {
						break
					}
					return false, err
				}

				os.Mkdir(folder, os.ModePerm)
				fname := filepath.Join(folder, filepath.Base(ent.Path))
				utils.Indent(log.Info, 2)(fmt.Sprintf("Extracting %s uid=%d, gid=%d, %s, %s to %s", ent.Mod, ent.Uid, ent.Gid, humanize.Bytes(uint64(ent.Size)), ent.Path, fname))
				err = ioutil.WriteFile(fname, fileBytes, 0644)
				if err != nil {
					return false, err
				}

				return true, nil
			}
		}

		rr.Seek(int64(ent.Size), io.SeekCurrent)
	}

	return false, nil
}
