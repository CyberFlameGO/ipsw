package dyld

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"
	"github.com/blacktop/go-macho/pkg/codesign"
	ctypes "github.com/blacktop/go-macho/pkg/codesign/types"
	"github.com/blacktop/go-macho/pkg/trie"
	mtypes "github.com/blacktop/go-macho/types"
	"github.com/blacktop/ipsw/internal/utils"
	"github.com/pkg/errors"
)

// Known good magic
var knownMagic = []string{
	"dyld_v1    i386",
	"dyld_v1  x86_64",
	"dyld_v1 x86_64h",
	"dyld_v1   armv5",
	"dyld_v1   armv6",
	"dyld_v1   armv7",
	"dyld_v1  armv7",
	"dyld_v1   arm64",
	"dyld_v1arm64_32",
	"dyld_v1  arm64e",
}

type localSymbolInfo struct {
	CacheLocalSymbolsInfo
	NListFileOffset   uint32
	NListByteSize     uint32
	StringsFileOffset uint32
}

type cacheImages []*CacheImage
type cacheMappings []*CacheMapping
type cacheMappingsWithSlideInfo []*CacheMappingWithSlideInfo
type codesignature *ctypes.CodeSignature

// A File represents an open dyld file.
type File struct {
	UUID    mtypes.UUID
	Headers map[mtypes.UUID]CacheHeader

	ByteOrder binary.ByteOrder

	Mappings              map[mtypes.UUID]cacheMappings
	MappingsWithSlideInfo map[mtypes.UUID]cacheMappingsWithSlideInfo

	Images cacheImages

	SlideInfo       slideInfo
	PatchInfo       CachePatchInfo
	LocalSymInfo    localSymbolInfo
	AcceleratorInfo CacheAcceleratorInfo

	BranchPools    []uint64
	CodeSignatures map[mtypes.UUID]codesignature

	AddressToSymbol map[uint64]string

	IsDyld4 bool
	symUUID mtypes.UUID

	r       map[mtypes.UUID]io.ReaderAt
	closers map[mtypes.UUID]io.Closer
}

// FormatError is returned by some operations if the data does
// not have the correct format for an object file.
type FormatError struct {
	off int64
	msg string
	val interface{}
}

func (e *FormatError) Error() string {
	msg := e.msg
	if e.val != nil {
		msg += fmt.Sprintf(" '%v'", e.val)
	}
	msg += fmt.Sprintf(" in record at byte %#x", e.off)
	return msg
}

func getUUID(r io.ReaderAt) (mtypes.UUID, error) {
	var uuidBytes [16]byte
	var badUUID mtypes.UUID

	if _, err := r.ReadAt(uuidBytes[0:], 0x58); err != nil {
		return badUUID, err
	}

	uuid := mtypes.UUID(uuidBytes)

	if uuid.IsNull() {
		return badUUID, fmt.Errorf("file's UUID is empty") // FIXME: should this actually stop or continue
	}

	return uuid, nil
}

// Open opens the named file using os.Open and prepares it for use as a dyld binary.
func Open(name string) (*File, error) {

	log.WithFields(log.Fields{
		"cache": name,
	}).Debug("Parsing Cache")
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	ff, err := NewFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	if ff.Headers[ff.UUID].ImagesOffset == 0 && ff.Headers[ff.UUID].ImagesCount == 0 {

		ff.IsDyld4 = true // NEW iOS15 dyld4 style caches

		for i := 1; i <= int(ff.Headers[ff.UUID].NumSubCaches); i++ {
			log.WithFields(log.Fields{
				"cache": fmt.Sprintf("%s.%d", name, i),
			}).Debug("Parsing SubCache")
			fsub, err := os.Open(fmt.Sprintf("%s.%d", name, i))
			if err != nil {
				return nil, err
			}

			uuid, err := getUUID(fsub)
			if err != nil {
				return nil, err
			}

			ff.parseCache(fsub, uuid)

			ff.closers[uuid] = fsub

			// if ff.Headers[uuid].SubCachesUUID != ff.Headers[ff.UUID].SubCachesUUID { FIXME: what IS this field actually?
			// 	return nil, fmt.Errorf("sub cache %s did not match expected UUID: %#x, got: %#x", fmt.Sprintf("%s.%d", name, i),
			// 		ff.Headers[ff.UUID].SubCachesUUID,
			// 		ff.Headers[uuid].SubCachesUUID)
			// }
		}

		if !ff.Headers[ff.UUID].SymbolsSubCacheUUID.IsNull() {
			log.WithFields(log.Fields{
				"cache": name + ".symbols",
			}).Debug("Parsing SubCache")
			fsym, err := os.Open(name + ".symbols")
			if err != nil {
				return nil, err
			}

			uuid, err := getUUID(fsym)
			if err != nil {
				return nil, err
			}

			if uuid != ff.Headers[ff.UUID].SymbolsSubCacheUUID {
				return nil, fmt.Errorf("%s.symbols UUID %s did NOT match expected UUID %s", name, uuid, ff.Headers[ff.UUID].SymbolsSubCacheUUID)
			}

			ff.symUUID = uuid // FIXME: what if there IS no .symbols like on M1 macOS

			ff.parseCache(fsym, uuid)

			ff.closers[uuid] = fsym
		}
	}

	ff.closers[ff.UUID] = f

	return ff, nil
}

// Close closes the File.
// If the File was created using NewFile directly instead of Open,
// Close has no effect.
func (f *File) Close() error {
	var err error
	for uuid, closer := range f.closers {
		if closer != nil {
			err = closer.Close()
			f.closers[uuid] = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadHeader opens a given cache and returns the dyld_shared_cache header
// func parseSubCache(name string) error {
// 	var header CacheHeader

// 	cache, err := os.Open(path)
// 	if err != nil {
// 		return nil, err
// 	}

// 	if err := binary.Read(cache, binary.LittleEndian, &header); err != nil {
// 		return nil, err
// 	}

// 	return &header, nil
// }

// NewFile creates a new File for accessing a dyld binary in an underlying reader.
// The dyld binary is expected to start at position 0 in the ReaderAt.
func NewFile(r io.ReaderAt) (*File, error) {

	f := new(File)

	// init all maps
	f.Headers = make(map[mtypes.UUID]CacheHeader)
	f.Mappings = make(map[mtypes.UUID]cacheMappings)
	f.MappingsWithSlideInfo = make(map[mtypes.UUID]cacheMappingsWithSlideInfo)
	f.CodeSignatures = make(map[mtypes.UUID]codesignature)
	f.r = make(map[mtypes.UUID]io.ReaderAt)
	f.closers = make(map[mtypes.UUID]io.Closer)
	f.AddressToSymbol = make(map[uint64]string, 7000000)

	// Read and decode dyld magic
	var ident [16]byte
	if _, err := r.ReadAt(ident[0:], 0); err != nil {
		return nil, err
	}
	// Verify magic
	if !utils.StrSliceContains(knownMagic, string(ident[:16])) {
		return nil, &FormatError{0, "invalid magic number", nil}
	}

	f.ByteOrder = binary.LittleEndian

	var uuidBytes [16]byte
	if _, err := r.ReadAt(uuidBytes[0:], 0x58); err != nil {
		return nil, err
	}

	f.UUID = mtypes.UUID(uuidBytes)

	if f.UUID.IsNull() {
		return nil, fmt.Errorf("file's UUID is empty") // FIXME: should this actually stop or continue
	}

	if err := f.parseCache(r, f.UUID); err != nil {
		return nil, fmt.Errorf("failed to parse cache %s: %v", f.UUID, err)
	}

	return f, nil
}

// parseCache parses dyld shared cache file
func (f *File) parseCache(r io.ReaderAt, uuid mtypes.UUID) error {

	sr := io.NewSectionReader(r, 0, 1<<63-1)

	// Read and decode dyld magic
	var ident [16]byte
	if _, err := r.ReadAt(ident[0:], 0); err != nil {
		return err
	}
	// Verify magic
	if !utils.StrSliceContains(knownMagic, string(ident[:16])) {
		return &FormatError{0, "invalid magic number", nil}
	}

	f.r[uuid] = r

	// Read entire file header.
	var hdr CacheHeader
	if err := binary.Read(sr, f.ByteOrder, &hdr); err != nil {
		return err
	}
	f.Headers[uuid] = hdr

	// Read dyld mappings.
	sr.Seek(int64(f.Headers[uuid].MappingOffset), os.SEEK_SET)

	for i := uint32(0); i != f.Headers[uuid].MappingCount; i++ {
		cmInfo := CacheMappingInfo{}
		if err := binary.Read(sr, f.ByteOrder, &cmInfo); err != nil {
			return err
		}
		cm := &CacheMapping{CacheMappingInfo: cmInfo}
		if cmInfo.InitProt.Execute() {
			cm.Name = "__TEXT"
		} else if cmInfo.InitProt.Write() {
			cm.Name = "__DATA"
		} else if cmInfo.InitProt.Read() {
			cm.Name = "__LINKEDIT"
		}
		f.Mappings[uuid] = append(f.Mappings[uuid], cm)
	}

	/***********************
	 * Read dyld slide info
	 ***********************/
	if f.Headers[uuid].SlideInfoOffsetUnused > 0 {
		f.ParseSlideInfo(uuid, CacheMappingAndSlideInfo{
			Address:         f.Mappings[uuid][1].Address,
			Size:            f.Mappings[uuid][1].Size,
			FileOffset:      f.Mappings[uuid][1].FileOffset,
			SlideInfoOffset: f.Headers[uuid].SlideInfoOffsetUnused,
			SlideInfoSize:   f.Headers[uuid].SlideInfoSizeUnused,
		}, false)
	} else {
		// Read NEW (in iOS 14) dyld mappings with slide info.
		sr.Seek(int64(f.Headers[uuid].MappingWithSlideOffset), os.SEEK_SET)
		for i := uint32(0); i != f.Headers[uuid].MappingWithSlideCount; i++ {
			cxmInfo := CacheMappingAndSlideInfo{}
			if err := binary.Read(sr, f.ByteOrder, &cxmInfo); err != nil {
				return err
			}

			cm := &CacheMappingWithSlideInfo{CacheMappingAndSlideInfo: cxmInfo, Name: "UNKNOWN"}

			if cxmInfo.MaxProt.Execute() {
				cm.Name = "__TEXT"
			} else if cxmInfo.MaxProt.Write() {
				if cm.Flags.IsAuthData() {
					cm.Name = "__AUTH"
				} else {
					cm.Name = "__DATA"
				}
				if cm.Flags.IsDirtyData() {
					cm.Name += "_DIRTY"
				} else if cm.Flags.IsConstData() {
					cm.Name += "_CONST"
				}
			} else if cxmInfo.InitProt.Read() {
				cm.Name = "__LINKEDIT"
			}

			if cm.SlideInfoSize > 0 {
				f.ParseSlideInfo(uuid, cm.CacheMappingAndSlideInfo, false)
			}

			f.MappingsWithSlideInfo[uuid] = append(f.MappingsWithSlideInfo[uuid], cm)
		}
	}

	// Read dyld images.
	var imagesCount uint32
	if f.Headers[uuid].ImagesOffset > 0 {
		imagesCount = f.Headers[uuid].ImagesCount
		sr.Seek(int64(f.Headers[uuid].ImagesOffset), os.SEEK_SET)
	} else {
		imagesCount = f.Headers[uuid].ImagesWithSubCachesCount
		sr.Seek(int64(f.Headers[uuid].ImagesWithSubCachesOffset), os.SEEK_SET)
	}

	if len(f.Images) == 0 {
		for i := uint32(0); i != imagesCount; i++ {
			iinfo := CacheImageInfo{}
			if err := binary.Read(sr, f.ByteOrder, &iinfo); err != nil {
				return fmt.Errorf("failed to read %T: %v", iinfo, err)
			}
			f.Images = append(f.Images, &CacheImage{
				Index: i,
				Info:  iinfo,
				cache: f,
			})
		}
		for idx, image := range f.Images {
			sr.Seek(int64(image.Info.PathFileOffset), os.SEEK_SET)
			r := bufio.NewReader(sr)
			if name, err := r.ReadString(byte(0)); err == nil {
				f.Images[idx].Name = fmt.Sprintf("%s", bytes.Trim([]byte(name), "\x00"))
			}
			// if offset, err := f.GetOffset(image.Info.Address); err == nil {
			// 	f.Images[idx].CacheLocalSymbolsEntry.DylibOffset = offset
			// }
		}
	}
	for idx, img := range f.Images {
		if f.IsAddressInCache(uuid, img.Info.Address) {
			f.Images[idx].cuuid = uuid
		}
	}

	// Read dyld code signature.
	sr.Seek(int64(f.Headers[uuid].CodeSignatureOffset), os.SEEK_SET)

	cs := make([]byte, f.Headers[uuid].CodeSignatureSize)
	if err := binary.Read(sr, f.ByteOrder, &cs); err != nil {
		return err
	}

	csig, err := codesign.ParseCodeSignature(cs)
	if err != nil {
		return err
	}
	f.CodeSignatures[uuid] = csig

	// Read dyld local symbol entries.
	if f.Headers[uuid].LocalSymbolsOffset != 0 {
		sr.Seek(int64(f.Headers[uuid].LocalSymbolsOffset), os.SEEK_SET)

		if err := binary.Read(sr, f.ByteOrder, &f.LocalSymInfo.CacheLocalSymbolsInfo); err != nil {
			return err
		}

		if f.Is64bit() {
			f.LocalSymInfo.NListByteSize = f.LocalSymInfo.NlistCount * 16
		} else {
			f.LocalSymInfo.NListByteSize = f.LocalSymInfo.NlistCount * 12
		}
		f.LocalSymInfo.NListFileOffset = uint32(f.Headers[uuid].LocalSymbolsOffset) + f.LocalSymInfo.NlistOffset
		f.LocalSymInfo.StringsFileOffset = uint32(f.Headers[uuid].LocalSymbolsOffset) + f.LocalSymInfo.StringsOffset

		sr.Seek(int64(f.Headers[uuid].LocalSymbolsOffset+uint64(f.LocalSymInfo.EntriesOffset)), os.SEEK_SET)

		for i := 0; i < int(f.LocalSymInfo.EntriesCount); i++ {
			// if err := binary.Read(sr, f.ByteOrder, &f.Images[i].CacheLocalSymbolsEntry); err != nil {
			// 	return nil, err
			// }
			var localSymEntry CacheLocalSymbolsEntry
			if f.Headers[uuid].ImagesOffset == 0 && f.Headers[uuid].ImagesCount == 0 { // NEW iOS15 dyld4 style caches
				if err := binary.Read(sr, f.ByteOrder, &localSymEntry); err != nil {
					return err
				}
			} else {
				var preDyld4LSEntry preDyld4cacheLocalSymbolsEntry
				if err := binary.Read(sr, f.ByteOrder, &preDyld4LSEntry); err != nil {
					return err
				}
				localSymEntry.DylibOffset = uint64(preDyld4LSEntry.DylibOffset)
				localSymEntry.NlistStartIndex = preDyld4LSEntry.NlistStartIndex
				localSymEntry.NlistCount = preDyld4LSEntry.NlistCount
			}

			if len(f.Images) > i {
				f.Images[i].CacheLocalSymbolsEntry = localSymEntry
			} else {
				f.Images = append(f.Images, &CacheImage{
					Index: uint32(i),
					// Info:      iinfo,
					cache:                  f,
					CacheLocalSymbolsEntry: localSymEntry,
				})
			}
			// f.Images[i].ReaderAt = io.NewSectionReader(r, int64(f.Images[i].DylibOffset), 1<<63-1)
		}
	}

	// Read dyld branch pool.
	if f.Headers[uuid].BranchPoolsOffset != 0 {
		sr.Seek(int64(f.Headers[uuid].BranchPoolsOffset), os.SEEK_SET)

		var bPools []uint64
		bpoolBytes := make([]byte, 8)
		for i := uint32(0); i != f.Headers[uuid].BranchPoolsCount; i++ {
			if err := binary.Read(sr, f.ByteOrder, &bpoolBytes); err != nil {
				return err
			}
			bPools = append(bPools, binary.LittleEndian.Uint64(bpoolBytes))
		}
		f.BranchPools = bPools
	}

	// Read dyld optimization info.
	if f.Headers[uuid].AccelerateInfoAddr != 0 {
		for _, mapping := range f.Mappings[uuid] {
			if mapping.Address <= f.Headers[uuid].AccelerateInfoAddr && f.Headers[uuid].AccelerateInfoAddr < mapping.Address+mapping.Size {
				accelInfoPtr := int64(f.Headers[uuid].AccelerateInfoAddr - mapping.Address + mapping.FileOffset)
				sr.Seek(accelInfoPtr, os.SEEK_SET)
				if err := binary.Read(sr, f.ByteOrder, &f.AcceleratorInfo); err != nil {
					return err
				}
				// Read dyld 16-bit array of sorted image indexes.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.BottomUpListOffset), os.SEEK_SET)
				bottomUpList := make([]uint16, f.AcceleratorInfo.ImageExtrasCount)
				if err := binary.Read(sr, f.ByteOrder, &bottomUpList); err != nil {
					return err
				}
				// Read dyld 16-bit array of dependencies.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.DepListOffset), os.SEEK_SET)
				depList := make([]uint16, f.AcceleratorInfo.DepListCount)
				if err := binary.Read(sr, f.ByteOrder, &depList); err != nil {
					return err
				}
				// Read dyld 16-bit array of re-exports.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.ReExportListOffset), os.SEEK_SET)
				reExportList := make([]uint16, f.AcceleratorInfo.ReExportCount)
				if err := binary.Read(sr, f.ByteOrder, &reExportList); err != nil {
					return err
				}
				// Read dyld image info extras.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.ImagesExtrasOffset), os.SEEK_SET)
				for i := uint32(0); i != f.AcceleratorInfo.ImageExtrasCount; i++ {
					imgXtrInfo := CacheImageInfoExtra{}
					if err := binary.Read(sr, f.ByteOrder, &imgXtrInfo); err != nil {
						return err
					}
					f.Images[i].CacheImageInfoExtra = imgXtrInfo
				}
				// Read dyld initializers list.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.InitializersOffset), os.SEEK_SET)
				for i := uint32(0); i != f.AcceleratorInfo.InitializersCount; i++ {
					accelInit := CacheAcceleratorInitializer{}
					if err := binary.Read(sr, f.ByteOrder, &accelInit); err != nil {
						return err
					}
					// fmt.Printf("  image[%3d] 0x%X\n", accelInit.ImageIndex, f.Mappings[0].Address+uint64(accelInit.FunctionOffset))
					f.Images[accelInit.ImageIndex].Initializer = f.Mappings[uuid][0].Address + uint64(accelInit.FunctionOffset)
				}
				// Read dyld DOF sections list.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.DofSectionsOffset), os.SEEK_SET)
				for i := uint32(0); i != f.AcceleratorInfo.DofSectionsCount; i++ {
					accelDOF := CacheAcceleratorDof{}
					if err := binary.Read(sr, f.ByteOrder, &accelDOF); err != nil {
						return err
					}
					// fmt.Printf("  image[%3d] 0x%X -> 0x%X\n", accelDOF.ImageIndex, accelDOF.SectionAddress, accelDOF.SectionAddress+uint64(accelDOF.SectionSize))
					f.Images[accelDOF.ImageIndex].DOFSectionAddr = accelDOF.SectionAddress
					f.Images[accelDOF.ImageIndex].DOFSectionSize = accelDOF.SectionSize
				}
				// Read dyld offset to start of ss.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.RangeTableOffset), os.SEEK_SET)
				for i := uint32(0); i != f.AcceleratorInfo.RangeTableCount; i++ {
					rEntry := CacheRangeEntry{}
					if err := binary.Read(sr, f.ByteOrder, &rEntry); err != nil {
						return err
					}
					// fmt.Printf("  0x%X -> 0x%X %s\n", rangeEntry.StartAddress, rangeEntry.StartAddress+uint64(rangeEntry.Size), f.Images[rangeEntry.ImageIndex].Name)
					offset, err := f.GetOffsetForUUID(uuid, rEntry.StartAddress)
					if err != nil {
						return errors.Wrap(err, "failed to get range entry's file offset")
					}
					f.Images[rEntry.ImageIndex].RangeEntries = append(f.Images[rEntry.ImageIndex].RangeEntries, rangeEntry{
						StartAddr:  rEntry.StartAddress,
						FileOffset: offset,
						Size:       rEntry.Size,
					})
				}
				// Read dyld trie containing all dylib paths.
				sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.DylibTrieOffset), os.SEEK_SET)
				dylibTrie := make([]byte, f.AcceleratorInfo.DylibTrieSize)
				if err := binary.Read(sr, f.ByteOrder, &dylibTrie); err != nil {
					return err
				}
			}
		}
	}

	// Read dyld text_info entries.
	sr.Seek(int64(f.Headers[uuid].ImagesTextOffset), os.SEEK_SET)
	for i := uint64(0); i != f.Headers[uuid].ImagesTextCount; i++ {
		if err := binary.Read(sr, f.ByteOrder, &f.Images[i].CacheImageTextInfo); err != nil {
			return err
		}
	}

	return nil
}

// ParseSlideInfo parses dyld slide info
func (f *File) ParseSlideInfo(uuid mtypes.UUID, mapping CacheMappingAndSlideInfo, dump bool) error {
	sr := io.NewSectionReader(f.r[uuid], 0, 1<<63-1)

	sr.Seek(int64(mapping.SlideInfoOffset), os.SEEK_SET)

	slideInfoVersionData := make([]byte, 4)
	sr.Read(slideInfoVersionData)
	slideInfoVersion := binary.LittleEndian.Uint32(slideInfoVersionData)

	sr.Seek(int64(mapping.SlideInfoOffset), os.SEEK_SET)

	switch slideInfoVersion {
	case 1:
		slideInfo := CacheSlideInfo{}
		if err := binary.Read(sr, f.ByteOrder, &slideInfo); err != nil {
			return err
		}

		if f.SlideInfo != nil {
			if f.SlideInfo.GetVersion() != slideInfo.GetVersion() {
				return fmt.Errorf("found mixed slide info versions: %d and %d", f.SlideInfo.GetVersion(), slideInfo.GetVersion())
			}
		}

		f.SlideInfo = slideInfo

		if dump {
			fmt.Printf("slide info version = %d\n", slideInfo.Version)
			fmt.Printf("toc_count          = %d\n", slideInfo.TocCount)
			fmt.Printf("data page count    = %d\n", mapping.Size/4096)

			sr.Seek(int64(mapping.SlideInfoOffset+uint64(slideInfo.EntriesOffset)), os.SEEK_SET)
			entries := make([]CacheSlideInfoEntry, int(slideInfo.EntriesCount))
			if err := binary.Read(sr, binary.LittleEndian, &entries); err != nil {
				return err
			}

			sr.Seek(int64(mapping.SlideInfoOffset+uint64(slideInfo.TocOffset)), os.SEEK_SET)
			tocs := make([]uint16, int(slideInfo.TocCount))
			if err := binary.Read(sr, binary.LittleEndian, &tocs); err != nil {
				return err
			}

			for i, toc := range tocs {
				fmt.Printf("%#08x: [% 5d,% 5d] ", int(mapping.Address)+i*4096, i, tocs[i])
				for j := 0; i < int(slideInfo.EntriesSize); i++ {
					fmt.Printf("%02x", entries[toc].bits[j])
				}
				fmt.Printf("\n")
			}
		}
	case 2:
		slideInfo := CacheSlideInfo2{}
		if err := binary.Read(sr, f.ByteOrder, &slideInfo); err != nil {
			return err
		}

		if f.SlideInfo != nil {
			if f.SlideInfo.GetVersion() != slideInfo.GetVersion() {
				return fmt.Errorf("found mixed slide info versions: %d and %d", f.SlideInfo.GetVersion(), slideInfo.GetVersion())
			}
		}

		f.SlideInfo = slideInfo

		if dump {
			fmt.Printf("slide info version = %d\n", slideInfo.Version)
			fmt.Printf("page_size          = %d\n", slideInfo.PageSize)
			fmt.Printf("delta_mask         = %#016x\n", slideInfo.DeltaMask)
			fmt.Printf("value_add          = %#x\n", slideInfo.ValueAdd)
			fmt.Printf("page_starts_count  = %d\n", slideInfo.PageStartsCount)
			fmt.Printf("page_extras_count  = %d\n", slideInfo.PageExtrasCount)

			var targetValue uint64
			var pointer uint64

			starts := make([]uint16, slideInfo.PageStartsCount)
			if err := binary.Read(sr, binary.LittleEndian, &starts); err != nil {
				return err
			}

			sr.Seek(int64(mapping.SlideInfoOffset+uint64(slideInfo.PageExtrasOffset)), os.SEEK_SET)
			extras := make([]uint16, int(slideInfo.PageExtrasCount))
			if err := binary.Read(sr, binary.LittleEndian, &extras); err != nil {
				return err
			}

			for i, start := range starts {
				// pageAddress := mapping.Address + uint64(uint32(i)*slideInfo.PageSize)
				pageOffset := mapping.FileOffset + uint64(uint32(i)*slideInfo.PageSize)
				rebaseChain := func(pageContent uint64, startOffset uint16) error {
					deltaShift := uint64(bits.TrailingZeros64(slideInfo.DeltaMask) - 2)
					pageOffset := uint32(startOffset)
					delta := uint32(1)
					for delta != 0 {
						sr.Seek(int64(pageContent+uint64(pageOffset)), os.SEEK_SET)
						if err := binary.Read(sr, binary.LittleEndian, &pointer); err != nil {
							return err
						}

						delta = uint32(pointer & slideInfo.DeltaMask >> deltaShift)
						targetValue = slideInfo.SlidePointer(pointer)

						var symName string
						sym, ok := f.AddressToSymbol[targetValue]
						if !ok {
							symName = "?"
						} else {
							symName = sym
						}

						fmt.Printf("    [% 5d + %#04x]: %#016x = %#016x, sym: %s\n", i, pageOffset, pointer, targetValue, symName)
						pageOffset += delta
					}

					return nil
				}

				if start == DYLD_CACHE_SLIDE_PAGE_ATTR_NO_REBASE {
					fmt.Printf("page[% 5d]: no rebasing\n", i)
				} else if start&DYLD_CACHE_SLIDE_PAGE_ATTR_EXTRA != 0 {
					fmt.Printf("page[% 5d]: ", i)
					j := start & 0x3FFF
					done := false
					for !done {
						aStart := extras[j]
						fmt.Printf("start=%#04x ", aStart&0x3FFF)
						pageStartOffset := (aStart & 0x3FFF) * 4
						rebaseChain(pageOffset, pageStartOffset)
						done = (extras[j] & DYLD_CACHE_SLIDE_PAGE_ATTR_END) != 0
						j++
					}
					fmt.Printf("\n")
				} else {
					fmt.Printf("page[% 5d]: start=0x%04X\n", i, starts[i])
					rebaseChain(pageOffset, start*4)
				}
			}
		}
	case 3:
		slideInfo := CacheSlideInfo3{}
		if err := binary.Read(sr, binary.LittleEndian, &slideInfo); err != nil {
			return err
		}

		if f.SlideInfo != nil {
			if f.SlideInfo.GetVersion() != slideInfo.GetVersion() {
				return fmt.Errorf("found mixed slide info versions: %d and %d", f.SlideInfo.GetVersion(), slideInfo.GetVersion())
			}
		}

		f.SlideInfo = slideInfo

		if dump {
			fmt.Printf("slide info version = %d\n", slideInfo.Version)
			fmt.Printf("page_size          = %d\n", slideInfo.PageSize)
			fmt.Printf("page_starts_count  = %d\n", slideInfo.PageStartsCount)
			fmt.Printf("auth_value_add     = %#016x\n", slideInfo.AuthValueAdd)

			var targetValue uint64
			var pointer CacheSlidePointer3

			PageStarts := make([]uint16, slideInfo.PageStartsCount)
			if err := binary.Read(sr, binary.LittleEndian, &PageStarts); err != nil {
				return err
			}

			for i, start := range PageStarts {
				pageAddress := mapping.Address + uint64(uint32(i)*slideInfo.PageSize)
				pageOffset := mapping.FileOffset + uint64(uint32(i)*slideInfo.PageSize)

				delta := uint64(start)

				if delta == DYLD_CACHE_SLIDE_V3_PAGE_ATTR_NO_REBASE {
					fmt.Printf("page[% 5d]: no rebasing\n", i)
					continue
				}

				fmt.Printf("page[% 5d]: start=0x%04X\n", i, delta)

				rebaseLocation := pageOffset

				for {
					rebaseLocation += delta

					sr.Seek(int64(pageOffset+delta), os.SEEK_SET)
					if err := binary.Read(sr, binary.LittleEndian, &pointer); err != nil {
						return err
					}

					if pointer.Authenticated() {
						// fmt.Println(fixupchains.DyldChainedPtrArm64eAuthBind{Pointer: pointer.Raw()}.String())
						targetValue = slideInfo.AuthValueAdd + pointer.OffsetFromSharedCacheBase()
					} else {
						// fmt.Println(fixupchains.DyldChainedPtrArm64eRebase{Pointer: pointer.Raw()}.String())
						targetValue = pointer.SignExtend51()
					}

					var symName string
					sym, ok := f.AddressToSymbol[targetValue]
					if !ok {
						symName = "?"
					} else {
						symName = sym
					}

					fmt.Printf("    [% 5d + 0x%04X] (%#x @ offset %#x => %#x) %s, sym: %s\n", i, (uint64)(rebaseLocation-pageOffset), (uint64)(pageAddress+delta), (uint64)(pageOffset+delta), targetValue, pointer, symName)

					if pointer.OffsetToNextPointer() == 0 {
						break
					}

					delta += pointer.OffsetToNextPointer() * 8
				}
			}
		}
	case 4:
		slideInfo := CacheSlideInfo4{}
		if err := binary.Read(sr, f.ByteOrder, &slideInfo); err != nil {
			return err
		}

		if f.SlideInfo != nil {
			if f.SlideInfo.GetVersion() != slideInfo.GetVersion() {
				return fmt.Errorf("found mixed slide info versions: %d and %d", f.SlideInfo.GetVersion(), slideInfo.GetVersion())
			}
		}

		f.SlideInfo = slideInfo

		if dump {
			fmt.Printf("slide info version = %d\n", slideInfo.Version)
			fmt.Printf("page_size          = %d\n", slideInfo.PageSize)
			fmt.Printf("delta_mask         = %#016x\n", slideInfo.DeltaMask)
			fmt.Printf("value_add          = %#016x\n", slideInfo.ValueAdd)
			fmt.Printf("page_starts_count  = %d\n", slideInfo.PageStartsCount)
			fmt.Printf("page_extras_count  = %d\n", slideInfo.PageExtrasCount)

			var targetValue uint64
			var pointer uint32

			starts := make([]uint16, slideInfo.PageStartsCount)
			if err := binary.Read(sr, binary.LittleEndian, &starts); err != nil {
				return err
			}

			sr.Seek(int64(mapping.SlideInfoOffset+uint64(slideInfo.PageExtrasOffset)), os.SEEK_SET)
			extras := make([]uint16, int(slideInfo.PageExtrasCount))
			if err := binary.Read(sr, binary.LittleEndian, &extras); err != nil {
				return err
			}

			for i, start := range starts {
				pageOffset := mapping.FileOffset + uint64(uint32(i)*slideInfo.PageSize)
				rebaseChainV4 := func(pageContent uint64, startOffset uint16) error {
					deltaShift := uint64(bits.TrailingZeros64(slideInfo.DeltaMask) - 2)
					pageOffset := uint32(startOffset)
					delta := uint32(1)
					for delta != 0 {
						sr.Seek(int64(pageContent+uint64(pageOffset)), os.SEEK_SET)
						if err := binary.Read(sr, binary.LittleEndian, &pointer); err != nil {
							return err
						}

						delta = uint32(uint64(pointer) & slideInfo.DeltaMask >> deltaShift)
						targetValue = slideInfo.SlidePointer(uint64(pointer))

						var symName string
						sym, ok := f.AddressToSymbol[targetValue]
						if !ok {
							symName = "?"
						} else {
							symName = sym
						}

						fmt.Printf("    [% 5d + %#04x]: %#08x = %#08x, sym: %s\n", i, pageOffset, pointer, targetValue, symName)
						pageOffset += delta
					}

					return nil
				}
				if start == DYLD_CACHE_SLIDE4_PAGE_NO_REBASE {
					fmt.Printf("page[% 5d]: no rebasing\n", i)
				} else if start&DYLD_CACHE_SLIDE4_PAGE_USE_EXTRA != 0 {
					fmt.Printf("page[% 5d]: ", i)
					j := (start & DYLD_CACHE_SLIDE4_PAGE_INDEX)
					done := false
					for !done {
						aStart := extras[j]
						fmt.Printf("start=0x%04X ", aStart&DYLD_CACHE_SLIDE4_PAGE_INDEX)
						pageStartOffset := (aStart & DYLD_CACHE_SLIDE4_PAGE_INDEX) * 4
						rebaseChainV4(pageOffset, pageStartOffset)
						done = (extras[j] & DYLD_CACHE_SLIDE4_PAGE_EXTRA_END) != 0
						j++
					}
					fmt.Printf("\n")
				} else {
					fmt.Printf("page[% 5d]: start=0x%04X\n", i, starts[i])
					rebaseChainV4(pageOffset, start*4)
				}
			}
		}

	default:
		log.Errorf("got unexpected dyld slide info version: %d", slideInfoVersion)
	}
	return nil
}

// ParsePatchInfo parses dyld patch info
func (f *File) ParsePatchInfo() error {
	if f.Headers[f.UUID].PatchInfoAddr > 0 {
		sr := io.NewSectionReader(f.r[f.UUID], 0, 1<<63-1)
		// Read dyld patch_info entries.
		patchInfoOffset, err := f.GetOffsetForUUID(f.UUID, f.Headers[f.UUID].PatchInfoAddr+8)
		if err != nil {
			return err
		}

		sr.Seek(int64(patchInfoOffset), io.SeekStart)
		if err := binary.Read(sr, f.ByteOrder, &f.PatchInfo); err != nil {
			return err
		}

		// Read all the other patch_info structs
		patchTableArrayOffset, err := f.GetOffsetForUUID(f.UUID, f.PatchInfo.PatchTableArrayAddr)
		if err != nil {
			return err
		}

		sr.Seek(int64(patchTableArrayOffset), io.SeekStart)
		imagePatches := make([]CacheImagePatches, f.PatchInfo.PatchTableArrayCount)
		if err := binary.Read(sr, f.ByteOrder, &imagePatches); err != nil {
			return err
		}

		patchExportNamesOffset, err := f.GetOffsetForUUID(f.UUID, f.PatchInfo.PatchExportNamesAddr)
		if err != nil {
			return err
		}

		exportNames := io.NewSectionReader(f.r[f.UUID], int64(patchExportNamesOffset), int64(f.PatchInfo.PatchExportNamesSize))

		patchExportArrayOffset, err := f.GetOffsetForUUID(f.UUID, f.PatchInfo.PatchExportArrayAddr)
		if err != nil {
			return err
		}

		sr.Seek(int64(patchExportArrayOffset), io.SeekStart)
		patchExports := make([]CachePatchableExport, f.PatchInfo.PatchExportArrayCount)
		if err := binary.Read(sr, f.ByteOrder, &patchExports); err != nil {
			return err
		}

		patchLocationArrayOffset, err := f.GetOffsetForUUID(f.UUID, f.PatchInfo.PatchLocationArrayAddr)
		if err != nil {
			return err
		}

		sr.Seek(int64(patchLocationArrayOffset), io.SeekStart)
		patchableLocations := make([]CachePatchableLocation, f.PatchInfo.PatchLocationArrayCount)
		if err := binary.Read(sr, f.ByteOrder, &patchableLocations); err != nil {
			return err
		}

		// Add patchabled export info to images
		for i, iPatch := range imagePatches {
			if iPatch.PatchExportsCount > 0 {
				for exportIndex := uint32(0); exportIndex != iPatch.PatchExportsCount; exportIndex++ {
					patchExport := patchExports[iPatch.PatchExportsStartIndex+exportIndex]
					var exportName string
					if uint64(patchExport.ExportNameOffset) < f.PatchInfo.PatchExportNamesSize {
						exportNames.Seek(int64(patchExport.ExportNameOffset), io.SeekStart)
						s, err := bufio.NewReader(exportNames).ReadString('\x00')
						if err != nil {
							return errors.Wrapf(err, "failed to read string at: %x", uint32(patchExportNamesOffset)+patchExport.ExportNameOffset)
						}
						exportName = strings.Trim(s, "\x00")
					} else {
						exportName = ""
					}
					plocs := make([]CachePatchableLocation, patchExport.PatchLocationsCount)
					for locationIndex := uint32(0); locationIndex != patchExport.PatchLocationsCount; locationIndex++ {
						plocs[locationIndex] = patchableLocations[patchExport.PatchLocationsStartIndex+locationIndex]
					}
					f.Images[i].PatchableExports = append(f.Images[i].PatchableExports, patchableExport{
						Name:           exportName,
						OffsetOfImpl:   patchExport.CacheOffsetOfImpl,
						PatchLocations: plocs,
					})
				}
			}
		}

		return nil
	}

	return fmt.Errorf("cache does NOT contain patch info")
}

// Image returns the Image with the given name, or nil if no such image exists.
func (f *File) Image(name string) *CacheImage {
	// fast path
	if idx, err := f.GetDylibIndex(name); err == nil {
		return f.Images[idx]
	}
	// slow path
	for _, i := range f.Images {
		if strings.EqualFold(strings.ToLower(i.Name), strings.ToLower(name)) {
			return i
		}
		if strings.EqualFold(strings.ToLower(filepath.Base(i.Name)), strings.ToLower(name)) {
			return i
		}
	}
	return nil
}

// GetImageContainingTextAddr returns a dylib whose __TEXT segment contains a given virtual address
// NOTE: this can be faster than GetImageContainingVMAddr as it avoids parsing the MachO
func (f *File) GetImageContainingTextAddr(addr uint64) (*CacheImage, error) {
	for _, img := range f.Images {
		if img.CacheImageTextInfo.LoadAddress <= addr && addr < img.CacheImageTextInfo.LoadAddress+uint64(img.TextSegmentSize) {
			return img, nil
		}
	}
	return nil, fmt.Errorf("address %#x not in any dylib __TEXT", addr)
}

// GetImageContainingVMAddr returns a dylib whose segment contains a given virtual address
func (f *File) GetImageContainingVMAddr(address uint64) (*CacheImage, error) {
	for _, img := range f.Images {
		m, err := img.GetPartialMacho()
		if err != nil {
			return nil, err
		}
		if seg := m.FindSegmentForVMAddr(address); seg != nil {
			return img, nil
		}
		m.Close()
	}
	return nil, fmt.Errorf("address %#x not in any dylib", address)
}

// // HasImagePath returns the index of a given image path
// func (f *File) HasImagePath(path string) (int, bool, error) {
// 	sr := io.NewSectionReader(f.r[f.UUID], 0, 1<<63-1)
// 	var imageIndex uint64
// 	for _, mapping := range f.Mappings[f.UUID] {
// 		if mapping.Address <= f.Headers[].AccelerateInfoAddr && f.AccelerateInfoAddr < mapping.Address+mapping.Size {
// 			accelInfoPtr := int64(f.AccelerateInfoAddr - mapping.Address + mapping.FileOffset)
// 			// Read dyld trie containing all dylib paths.
// 			sr.Seek(accelInfoPtr+int64(f.AcceleratorInfo.DylibTrieOffset), os.SEEK_SET)
// 			dylibTrie := make([]byte, f.AcceleratorInfo.DylibTrieSize)
// 			if err := binary.Read(sr, f.ByteOrder, &dylibTrie); err != nil {
// 				return 0, false, err
// 			}
// 			imageNode, err := trie.WalkTrie(dylibTrie, path)
// 			if err != nil {
// 				return 0, false, err
// 			}
// 			imageIndex, _, err = trie.ReadUleb128FromBuffer(bytes.NewBuffer(dylibTrie[imageNode:]))
// 			if err != nil {
// 				return 0, false, err
// 			}
// 		}
// 	}
// 	return int(imageIndex), true, nil
// }

// GetDylibIndex returns the index of a given dylib
func (f *File) GetDylibIndex(path string) (uint64, error) {
	sr := io.NewSectionReader(f.r[f.UUID], 0, 1<<63-1)

	if f.Headers[f.UUID].DylibsTrieAddr == 0 {
		return 0, fmt.Errorf("cache does not contain dylibs trie info")
	}

	off, err := f.GetOffsetForUUID(f.UUID, f.Headers[f.UUID].DylibsTrieAddr)
	if err != nil {
		return 0, err
	}

	sr.Seek(int64(off), io.SeekStart)

	dylibTrie := make([]byte, f.Headers[f.UUID].DylibsTrieSize)
	if err := binary.Read(sr, f.ByteOrder, &dylibTrie); err != nil {
		return 0, err
	}

	imageNode, err := trie.WalkTrie(dylibTrie, path)
	if err != nil {
		return 0, fmt.Errorf("dylib not found in dylibs trie")
	}

	imageIndex, _, err := trie.ReadUleb128FromBuffer(bytes.NewBuffer(dylibTrie[imageNode:]))
	if err != nil {
		return 0, fmt.Errorf("failed to read ULEB at image node in dyblibs trie")
	}

	return imageIndex, nil
}

// FindDlopenOtherImage returns the dlopen OtherImage for a given path
func (f *File) FindDlopenOtherImage(path string) (uint64, error) {
	sr := io.NewSectionReader(f.r[f.UUID], 0, 1<<63-1)

	if f.Headers[f.UUID].OtherTrieAddr == 0 {
		return 0, fmt.Errorf("cache does not contain dlopen other image trie info")
	}

	offset, err := f.GetOffsetForUUID(f.UUID, f.Headers[f.UUID].OtherTrieAddr)
	if err != nil {
		return 0, err
	}

	sr.Seek(int64(offset), io.SeekStart)

	otherTrie := make([]byte, f.Headers[f.UUID].OtherTrieSize)
	if err := binary.Read(sr, f.ByteOrder, &otherTrie); err != nil {
		return 0, err
	}

	imageNode, err := trie.WalkTrie(otherTrie, path)
	if err != nil {
		return 0, err
	}

	imageNum, _, err := trie.ReadUleb128FromBuffer(bytes.NewBuffer(otherTrie[imageNode:]))
	if err != nil {
		return 0, err
	}

	return imageNum, nil
}

// FindClosure returns the closure pointer for a given path
func (f *File) FindClosure(executablePath string) (uint64, error) {
	sr := io.NewSectionReader(f.r[f.UUID], 0, 1<<63-1)

	if f.Headers[f.UUID].ProgClosuresTrieAddr == 0 {
		return 0, fmt.Errorf("cache does not contain prog closures trie info")
	}

	offset, err := f.GetOffsetForUUID(f.UUID, f.Headers[f.UUID].ProgClosuresTrieAddr)
	if err != nil {
		return 0, err
	}

	sr.Seek(int64(offset), io.SeekStart)

	progClosuresTrie := make([]byte, f.Headers[f.UUID].ProgClosuresTrieSize)
	if err := binary.Read(sr, f.ByteOrder, &progClosuresTrie); err != nil {
		return 0, err
	}
	imageNode, err := trie.WalkTrie(progClosuresTrie, executablePath)
	if err != nil {
		return 0, err
	}
	closureOffset, _, err := trie.ReadUleb128FromBuffer(bytes.NewBuffer(progClosuresTrie[imageNode:]))
	if err != nil {
		return 0, err
	}

	return f.Headers[f.UUID].ProgClosuresAddr + closureOffset, nil
}
