package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/jstaf/onedriver/logger"
)

// DriveItemParent describes a DriveItem's parent in the Graph API (just another
// DriveItem's ID and its path)
type DriveItemParent struct {
	//TODO restructure to use its own ID functions
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	item *DriveItem
}

// Folder is used for parsing only
type Folder struct {
	ChildCount uint32 `json:"childCount,omitempty"`
}

// File is used for parsing only
type File struct {
	MimeType string `json:"mimeType,omitempty"`
}

// Deleted is used for detecting when items get deleted on the server
type Deleted struct {
	State string `json:"state,omitempty"`
}

// DriveItem represents a file or folder fetched from the Graph API. All struct
// fields are pointers so as to avoid including them when marshaling to JSON
// if not present. Fields named "xxxxxInternal" should never be accessed, they
// are there for JSON umarshaling/marshaling only. (They are not safe to access
// concurrently.) This struct's methods are thread-safe and can be called
// concurrently.
type DriveItem struct {
	nodefs.File      `json:"-"`
	uploadSession    *UploadSession   // current upload session, or nil
	auth             *Auth            // only populated for root item
	data             *[]byte          // empty by default
	hasChanges       bool             // used to trigger an upload on flush
	IDInternal       string           `json:"id,omitempty"`
	NameInternal     string           `json:"name,omitempty"`
	SizeInternal     uint64           `json:"size,omitempty"`
	ModTimeInternal  *time.Time       `json:"lastModifiedDatetime,omitempty"`
	mode             uint32           // do not set manually
	Parent           *DriveItemParent `json:"parentReference,omitempty"`
	children         map[string]*DriveItem
	subdir           uint32 // used purely by NLink()
	mutex            *sync.RWMutex
	Folder           *Folder  `json:"folder,omitempty"`
	FileInternal     *File    `json:"file,omitempty"`
	Deleted          *Deleted `json:"deleted,omitempty"`
	ConflictBehavior string   `json:"@microsoft.graph.conflictBehavior,omitempty"`
}

// NewDriveItem initializes a new DriveItem
func NewDriveItem(name string, mode uint32, parent *DriveItem) *DriveItem {
	var empty []byte
	currentTime := time.Now()
	parentID, _ := parent.ID(Auth{}) // we should have this already
	parent.mutex.RLock()
	defer parent.mutex.RUnlock()
	return &DriveItem{
		File:         nodefs.NewDefaultFile(),
		NameInternal: name,
		Parent: &DriveItemParent{
			ID:   parentID,
			Path: parent.Parent.Path + "/" + parent.Name(),
			item: parent,
		},
		children:        make(map[string]*DriveItem),
		mutex:           &sync.RWMutex{},
		data:            &empty,
		ModTimeInternal: &currentTime,
		mode:            mode,
	}
}

func (d DriveItem) String() string {
	length := d.Size()
	if length > 10 {
		length = 10
	}
	return fmt.Sprintf("DriveItem(%x)", (*d.data)[:length])
}

// Set an item's parent
func (d *DriveItem) setParent(newParent *DriveItem) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	id, _ := newParent.ID(Auth{})
	d.Parent = &DriveItemParent{
		ID:   id,
		Path: newParent.Path(),
		item: newParent,
	}
}

// Name is used to ensure we access a copy
func (d DriveItem) Name() string {
	return d.NameInternal
}

// SetName sets the name of the item in a thread-safe manner.
func (d *DriveItem) SetName(name string) {
	d.mutex.Lock()
	d.NameInternal = name
	d.mutex.Unlock()
}

// ID uploads an empty file to obtain a Onedrive ID if it doesn't already have
// one. This is necessary to avoid race conditions against uploads if the file
// has not already been uploaded. You can use an empty Auth object if you're
// sure that the item already has an ID or otherwise don't need to fetch an ID
// (such as when deleting an item that is only local).
func (d *DriveItem) ID(auth Auth) (string, error) {
	// copy the item so we can access it's ID without locking the item itself
	d.mutex.RLock()
	cpy := *d
	parentID := d.Parent.ID
	d.mutex.RUnlock()

	if cpy.IsDir() {
		//TODO add checks for directory, perhaps retry the dir creation again
		//server-side?
		return cpy.IDInternal, nil
	}

	if cpy.IDInternal == "" && auth.AccessToken != "" {
		uploadPath := fmt.Sprintf("/me/drive/items/%s:/%s:/content", parentID, cpy.Name())
		resp, err := Put(uploadPath, auth, strings.NewReader(""))
		if err != nil {
			if strings.Contains(err.Error(), "nameAlreadyExists") {
				// This likely got fired off just as an initial upload completed.
				// Check both our local copy and the server.

				// Do we have it (from another thread)?
				d.mutex.RLock()
				id := d.IDInternal
				path := d.Path()
				if id != "" {
					defer d.mutex.RUnlock()
					return id, nil
				}
				d.mutex.RUnlock()

				// Does the server have it?
				latest, err := GetItem(path, auth)
				if err == nil {
					// hooray!
					return latest.IDInternal, nil
				}
			}
			return "", err
		}

		// we use a new DriveItem to unmarshal things into or it will fuck
		// with the existing object (namely its size)
		unsafe := NewDriveItem(cpy.Name(), 0644, cpy.Parent.item)
		err = json.Unmarshal(resp, unsafe)
		if err != nil {
			return "", err
		}
		// this is all we really wanted from this transaction
		d.mutex.Lock()
		d.IDInternal = unsafe.IDInternal
		d.mutex.Unlock()
		return unsafe.IDInternal, nil
	}
	return cpy.IDInternal, nil
}

// Path returns an item's full Path
func (d DriveItem) Path() string {
	// special case when it's the root item
	if d.Parent.Path == "" && d.Name() == "root" {
		return "/"
	}

	// all paths come prefixed with "/drive/root:"
	prepath := strings.TrimPrefix(d.Parent.Path+"/"+d.Name(), "/drive/root:")
	return strings.Replace(prepath, "//", "/", -1)
}

// only used for parsing
type driveChildren struct {
	Children []*DriveItem `json:"value"`
}

// GetChildren fetches all DriveItems that are children of resource at path.
// Also initializes the children field.
func (d *DriveItem) GetChildren(auth Auth) (map[string]*DriveItem, error) {
	//TODO will exit prematurely if *any* children are in the cache
	if !d.IsDir() || d.children != nil {
		return d.children, nil
	}

	body, err := Get(ChildrenPath(d.Path()), auth)
	var fetched driveChildren
	if err != nil {
		return nil, err
	}
	json.Unmarshal(body, &fetched)

	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.children = make(map[string]*DriveItem)
	for _, child := range fetched.Children {
		child.Parent.item = d
		child.mutex = &sync.RWMutex{}
		if child.IsDir() {
			d.subdir++
		}
		d.children[strings.ToLower(child.Name())] = child
	}

	return d.children, nil
}

// FetchContent fetches a DriveItem's content and initializes the .Data field.
func (d *DriveItem) FetchContent(auth Auth) error {
	id, err := d.ID(auth)
	if err != nil {
		logger.Error("Could not obtain ID:", err.Error())
		return err
	}
	body, err := Get("/me/drive/items/"+id+"/content", auth)
	if err != nil {
		return err
	}
	d.mutex.Lock()
	d.data = &body
	d.File = nodefs.NewDefaultFile()
	d.mutex.Unlock()
	return nil
}

// Read from a DriveItem like a file
func (d DriveItem) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	end := int(off) + int(len(buf))
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	if end > len(*d.data) {
		end = len(*d.data)
	}
	logger.Tracef("%s: %d bytes at offset %d\n", d.Path(), int64(end)-off, off)
	return fuse.ReadResultData((*d.data)[off:end]), fuse.OK
}

// Write to a DriveItem like a file. Note that changes are 100% local until
// Flush() is called.
func (d *DriveItem) Write(data []byte, off int64) (uint32, fuse.Status) {
	nWrite := len(data)
	offset := int(off)
	logger.Tracef("%s: %d bytes at offset %d\n", d.Path(), nWrite, off)

	d.mutex.Lock()
	defer d.mutex.Unlock()
	if offset+nWrite > int(d.SizeInternal)-1 {
		// we've exceeded the file size, overwrite via append
		*d.data = append((*d.data)[:offset], data...)
	} else {
		// writing inside the current file, overwrite in place
		copy((*d.data)[offset:], data)
	}
	// probably a better way to do this, but whatever
	d.SizeInternal = uint64(len(*d.data))
	d.hasChanges = true

	return uint32(nWrite), fuse.OK
}

func (d DriveItem) getRoot() *DriveItem {
	parent := d.Parent.item
	for parent.Parent.Path != "" {
		parent = parent.Parent.item
	}
	return parent
}

// Flush is called when a file descriptor is closed. This is responsible for all
// uploads of file contents.
func (d *DriveItem) Flush() fuse.Status {
	logger.Trace(d.Path())
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.hasChanges {
		auth := *d.getRoot().auth
		d.hasChanges = false
		// ensureID() is no longer used here to make upload dispatch even faster
		// (since upload is using ensureID() internally)
		go d.Upload(auth)
	}
	return fuse.OK
}

// GetAttr returns a the DriveItem as a UNIX stat. Holds the read mutex for all
// of the "metadata fetch" operations.
func (d DriveItem) GetAttr(out *fuse.Attr) fuse.Status {
	out.Size = d.Size()
	out.Nlink = d.NLink()
	out.Atime = d.ModTime()
	out.Mtime = d.ModTime()
	out.Ctime = d.ModTime()
	out.Mode = d.Mode()
	out.Owner = fuse.Owner{
		Uid: uint32(os.Getuid()),
		Gid: uint32(os.Getgid()),
	}
	return fuse.OK
}

// Utimens sets the access/modify times of a file
func (d *DriveItem) Utimens(atime *time.Time, mtime *time.Time) fuse.Status {
	logger.Trace(d.Path())
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.ModTimeInternal = mtime
	return fuse.OK
}

// Truncate cuts a file in place
func (d *DriveItem) Truncate(size uint64) fuse.Status {
	logger.Trace(d.Path())
	d.mutex.Lock()
	defer d.mutex.Unlock()
	*d.data = (*d.data)[:size]
	d.SizeInternal = size
	d.hasChanges = true
	return fuse.OK
}

// IsDir returns if it is a directory (true) or file (false).
func (d DriveItem) IsDir() bool {
	// following statement returns 0 if the dir bit is not set
	return d.Mode()&fuse.S_IFDIR > 0
}

// Mode returns the permissions/mode of the file.
func (d DriveItem) Mode() uint32 {
	if d.mode == 0 { // only 0 if fetched from Graph API
		if d.FileInternal == nil { // nil if a folder
			d.mode = fuse.S_IFDIR | 0755
		} else {
			d.mode = fuse.S_IFREG | 0644
		}
	}
	return d.mode
}

// Chmod changes the mode of a file
func (d *DriveItem) Chmod(perms uint32) fuse.Status {
	logger.Trace(d.Path())
	d.mutex.Lock()
	if d.IsDir() {
		d.mode = fuse.S_IFDIR | perms
	} else {
		d.mode = fuse.S_IFREG | perms
	}
	d.mutex.Unlock()
	return fuse.OK
}

// ModTime returns the Unix timestamp of last modification (to get a time.Time
// struct, use time.Unix(int64(d.ModTime()), 0))
func (d DriveItem) ModTime() uint64 {
	return uint64(d.ModTimeInternal.Unix())
}

// NLink gives the number of hard links to an inode (or child count if a
// directory)
func (d DriveItem) NLink() uint32 {
	if d.IsDir() {
		d.mutex.RLock()
		defer d.mutex.RUnlock()
		// we precompute d.subdir due to mutex lock contention with NLink and
		// other ops. d.subdir is modified by cache Insert/Delete and GetChildren.
		return 2 + d.subdir
	}
	return 1
}

// Size pretends that folders are 4096 bytes, even though they're 0 (since
// they actually don't exist).
func (d DriveItem) Size() uint64 {
	if d.IsDir() {
		return 4096
	}
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.SizeInternal
}
