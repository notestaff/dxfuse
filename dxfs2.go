package dxfs2

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"sort"
	"strconv"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"

	// The dxda package has the get-environment code
	"github.com/dnanexus/dxda"

	"golang.org/x/net/context"
)

type FS struct {
	// configuration information for accessing dnanexus servers
	dxEnv dxda.DXEnvironment

	// File catalog. A fixed list of dx:files that are exposed by this mount point.
	catalog map[string]DxFileDesc

	uid uint32
	gid uint32
}

type Dir struct {
	fs    *FS
	path   string
}

type File struct {
	fs       *FS
	dxDesc   *DxDescribe
	inode     uint64
}

// A URL generated with the /file-xxxx/download API call, that is
// used to download file ranges.
type DxDownloadURL struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type FileHandle struct {
	f *File

	// URL used for downloading file ranges
	url DxDownloadURL
}

type DxFileDesc struct {
	dxDesc DxDescribe
	inode uint64
}

const BASE_FILE_INODE uint64 = 10

// Mount the filesystem:
//  - setup the debug log to the FUSE kernel log (I think)
//  - mount as read-only
func Mount(mountpoint string, dxEnv dxda.DXEnvironment, files map[string]DxDescribe) error {
	//log.Printf("mounting dxfs2\n")
	c, err := fuse.Mount(mountpoint, fuse.AllowOther(), fuse.ReadOnly(),
		fuse.MaxReadahead(1024 * 1024), fuse.AsyncRead())
	if err != nil {
		return err
	}
	defer c.Close()

	// get the Unix uid and gid
	user, err := user.Current()
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(user.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(user.Gid)
	if err != nil {
		return err
	}

	// set a mapping from file-id to its description.
	// Choose a stable inode for each file. It cannot change
	// during the filesystem lifetime.
	var inodeCnt uint64 = BASE_FILE_INODE
	catalog := make(map[string]DxFileDesc)
	for fid, dxDesc := range(files) {
		catalog[fid] = DxFileDesc {
			dxDesc : dxDesc,
			inode : inodeCnt,
		}
		inodeCnt++
	}

	filesys := &FS{
		dxEnv : dxEnv,
		catalog : catalog,
		uid : uint32(uid),
		gid : uint32(gid),
	}
	if err := fs.Serve(c, filesys); err != nil {
		return err
	}

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		return err
	}

	return nil
}

var _ fs.FS = (*FS)(nil)

func (f *FS) Root() (fs.Node, error) {
	//log.Printf("Get root directory\n")
	n := &Dir{
		fs : f,
		path : "/",
	}
	return n, nil
}

// Make sure that Dir implements the fs.Node interface
var _ fs.Node = (*Dir)(nil)


// We only support the root directory
func (dir *Dir) Attr(ctx context.Context, a *fuse.Attr) error {
	if dir.path != "/" {
		return fuse.ENOSYS;
	}
	// this can be retained in cache indefinitely (a year is an approximation)
	a.Valid = time.Until(time.Unix(1000 * 1000 * 1000, 0))
	a.Inode = 1
	a.Size = 4096  // dummy size
	a.Blocks = 8
	a.Atime = time.Now()
	a.Mtime = time.Now()
	a.Ctime = time.Now()
	a.Mode = os.ModeDir | 0777
	a.Nlink = 1
	a.Uid = dir.fs.uid
	a.Gid = dir.fs.uid
	a.BlockSize = 4 * 1024
	return nil
}

func (dir *Dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	//log.Printf("ReadDirAll dir=%s\n", dir.path)

	// create a directory entry for each of the file descriptions
	dEntries := make([]fuse.Dirent, 0, len(dir.fs.catalog))
	for key, fDesc := range dir.fs.catalog {
		dEntries = append(dEntries, fuse.Dirent{
			Inode : fDesc.inode,
			Type : fuse.DT_File,
			Name : key,
		})
	}
	sort.Slice(dEntries, func(i, j int) bool { return dEntries[i].Name < dEntries[j].Name })
	return dEntries, nil
}

var _ = fs.HandleReadDirAller(&Dir{})

var _ = fs.NodeRequestLookuper(&Dir{})

// We ignore the directory, because it is always the root of the filesystem.
func (dir *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fs.Node, error) {
	//log.Printf("Lookup dir=%s  filename=%s\n", dir.path, req.Name)

	// lookup in the in-memory catalog
	catEntry, ok := dir.fs.catalog[req.Name]
	if !ok {
		// file does not exist
		return nil, fuse.ENOENT
	}

	child := &File{
		fs: dir.fs,
		dxDesc: &catEntry.dxDesc,
		inode: catEntry.inode,
	}
	return child, nil
}

var _ fs.Node = (*File)(nil)

func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Size = f.dxDesc.Size
	//log.Printf("Attr  size=%d\n", a.Size)

	// because the platform has only immutable files, these
	// timestamps are all the same
	a.Mtime = f.dxDesc.Mtime
	a.Ctime = f.dxDesc.Ctime
	a.Crtime = f.dxDesc.Ctime
	a.Mode = 0400 // read only access
	a.Nlink = 1
	a.Uid = f.fs.uid
	a.Gid = f.fs.gid
	//a.BlockSize = 1024 * 1024
	return nil
}

var _ = fs.NodeOpener(&File{})

func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	// these files are read only
	if !req.Flags.IsReadOnly() {
		return nil, fuse.Errno(syscall.EACCES)
	}

	// create a download URL for this file
	const secondsInYear int = 60 * 60 * 24 * 365
	payload := fmt.Sprintf("{\"project\": \"%s\", \"duration\": %d}",
		f.dxDesc.ProjId, secondsInYear)

	body, err := DxAPI(&f.fs.dxEnv, fmt.Sprintf("%s/download", f.dxDesc.FileId), payload)
	if err != nil {
		return nil, err
	}
	var u DxDownloadURL
	json.Unmarshal(body, &u)

	fh := &FileHandle{
		f : f,
		url: u,
	}
	return fh, nil
}

var _ fs.Handle = (*FileHandle)(nil)

var _ fs.HandleReleaser = (*FileHandle)(nil)

func (fh *FileHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	// nothing to do
	return nil
}

var _ = fs.HandleReader(&FileHandle{})

func (fh *FileHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	headers := make(map[string]string)

	// Copy the immutable headers
	for key, value := range fh.url.Headers {
		headers[key] = value
	}

	// add an extent in the file that we want to read
	endOfs := req.Offset + int64(req.Size) - 1
	headers["Range"] = fmt.Sprintf("bytes=%d-%d", req.Offset, endOfs)
	//log.Printf("Read  ofs=%d  len=%d\n", req.Offset, req.Size)

	reqUrl := fh.url.URL + "/" + fh.f.dxDesc.ProjId
	body,err := DxHttpRequest("GET", reqUrl, headers, []byte("{}"))
	if err != nil {
		return err
	}

	resp.Data = body
	return nil
}
