package tusdx

import (
    "errors"
    "fmt"
    "github.com/Acconut/lockfile"
    "github.com/MaxTech/tusdx/tusdx_model"
    "github.com/google/uuid"
    "github.com/tus/tusd"
    "gopkg.in/yaml.v2"
    "io"
    "io/ioutil"
    "os"
    "path/filepath"
    "strings"
    "time"
)

var defaultFilePerm = os.FileMode(0664)

type EitStore struct {
    // Relative or absolute path to store files in. FileStore does not check
    // whether the path exists, use os.MkdirAll in this case on your own.
    Path string
}

// New creates a new file based storage backend. The directory specified will
// be used as the only storage entry. This method does not check
// whether the path exists, use os.MkdirAll to ensure.
// In addition, a locking mechanism is provided.
func New(path string) EitStore {
    return EitStore{path}
}

// UseIn sets this store as the core data store in the passed composer and adds
// all possible extension to it.
func (store EitStore) UseIn(composer *tusd.StoreComposer) {
    composer.UseCore(store)
    composer.UseGetReader(store)
    composer.UseTerminater(store)
    composer.UseLocker(store)
    composer.UseConcater(store)
    composer.UseLengthDeferrer(store)
}

// Create a new upload using the size as the file's length. The method must
// return an unique id which is used to identify the upload. If no backend
// (e.g. Riak) specifes the id you may want to use the uid package to
// generate one. The properties Size and MetaData will be filled.
func (store EitStore) NewUpload(info tusd.FileInfo) (id string, err error) {
    id = strings.Replace(uuid.New().String(), "-", "", -1)
    info.ID = id

    savePath := store.path(id)
    _, err = os.Stat(savePath)
    if os.IsNotExist(err) {
        err = os.MkdirAll(savePath, os.ModePerm)
        if err != nil {
            logger.Error("create dir error:", err.Error())
        }
    }

    // Create .bin file with no content
    file, err := os.OpenFile(store.binPath(id), os.O_CREATE|os.O_WRONLY, defaultFilePerm)
    if err != nil {
        if os.IsNotExist(err) {
            err = fmt.Errorf("upload directory does not exist: %s", store.Path)
        }
        return "", err
    }
    defer file.Close()

    // writeInfo creates the file by itself if necessary
    err = store.writeInfo(id, info)

    tusdFile := new(tusdx_model.TusdFilesModel)
    tusdFile.Id = info.ID
    tusdFile.FileName = info.MetaData["filename"]
    tusdFile.FilePath = savePath
    tusdFile.CreateTime = time.Now()
    intNum := tusdFilesDao.InsertOneByModel(tusdFile)
    if intNum > 0 {
        go setTusdFileToRedis(tusdFile)
    }

    return
}

// Write the chunk read from src into the file specified by the id at the
// given offset. The handler will take care of validating the offset and
// limiting the size of the src to not overflow the file's size. It may
// return an os.ErrNotExist which will be interpreted as a 404 Not Found.
// It will also lock resources while they are written to ensure only one
// write happens per time.
// The function call must return the number of bytes written.
func (store EitStore) WriteChunk(id string, offset int64, src io.Reader) (int64, error) {
    file, err := os.OpenFile(store.binPath(id), os.O_WRONLY|os.O_APPEND, defaultFilePerm)
    if err != nil {
        return 0, err
    }
    defer file.Close()

    n, err := io.Copy(file, src)
    return n, err
}

// Read the fileinformation used to validate the offset and respond to HEAD
// requests. It may return an os.ErrNotExist which will be interpreted as a
// 404 Not Found.
func (store EitStore) GetInfo(id string) (tusd.FileInfo, error) {
    info := tusd.FileInfo{}

    tusdFile := store.getTusdFile(id)
    if tusdFile == nil {
        return info, errors.New("file info not found")
    }

    infoPath := fmt.Sprintf("%s/%s.info", tusdFile.FilePath, id)
    data, err := ioutil.ReadFile(infoPath)
    if err != nil {
        return info, err
    }
    if err := yaml.Unmarshal(data, &info); err != nil {
        return info, err
    }

    binPath := fmt.Sprintf("%s/%s.bin", tusdFile.FilePath, id)
    stat, err := os.Stat(binPath)
    if err != nil {
        return info, err
    }

    info.Offset = stat.Size()

    return info, nil
}

// Terminate an upload so any further requests to the resource, both reading
// and writing, must return os.ErrNotExist or similar.
func (store EitStore) Terminate(id string) error {
    tusdFile := store.getTusdFile(id)
    if tusdFile != nil {
        tusdFilesDao.DeleteOneByModel(tusdFile)
    }

    infoPath := fmt.Sprintf("%s/%s.info", tusdFile.FilePath, id)
    if err := os.Remove(infoPath); err != nil {
        return err
    }

    binPath := fmt.Sprintf("%s/%s.bin", tusdFile.FilePath, id)
    if err := os.Remove(binPath); err != nil {
        return err
    }
    return nil
}

// FinishUpload executes additional operations for the finished upload which
// is specified by its ID.
func (store EitStore) FinishUpload(id string) error {
    tusdFile := store.getTusdFile(id)
    if tusdFile == nil {
        return errors.New("file info not found")
    }

    tusdFile.UploadStatus = 1
    updNum := tusdFilesDao.UpdateOneByModel(tusdFile)
    if updNum > 0 {
        go setTusdFileToRedis(tusdFile)
    }

    return nil
}

// LockUpload attempts to obtain an exclusive lock for the upload specified
// by its id.
// If this operation fails because the resource is already locked, the
// tusd.ErrFileLocked must be returned. If no error is returned, the attempt
// is consider to be successful and the upload to be locked until UnlockUpload
// is invoked for the same upload.
func (store EitStore) LockUpload(id string) error {
    lock, err := store.newLock(id)
    if err != nil {
        return err
    }

    _, err = os.Stat(string(lock))
    if err == nil {
        return tusd.ErrFileLocked
    }

    tmpLock, _ := os.OpenFile(string(lock), os.O_WRONLY|os.O_CREATE, os.ModePerm)

    _, err = io.WriteString(tmpLock, fmt.Sprintf("%d\n", os.Getpid()))

    //err = lock.TryLock()
    //if err == lockfile.ErrBusy {
    //    return tusd.ErrFileLocked
    //}

    return nil
}

// UnlockUpload releases an existing lock for the given upload.
func (store EitStore) UnlockUpload(id string) error {
    lock, err := store.newLock(id)
    if err != nil {
        return err
    }

    _, err = os.Stat(string(lock))
    // A "no such file or directory" will be returned if no lockfile was found.
    // Since this means that the file has never been locked, we drop the error
    // and continue as if nothing happened.
    if os.IsNotExist(err) {
        return nil
    }

    err = lock.Unlock()

    return err
}

// GetReader returns a reader which allows iterating of the content of an
// upload specified by its ID. It should attempt to provide a reader even if
// the upload has not been finished yet but it's not required.
// If the returned reader also implements the io.Closer interface, the
// Close() method will be invoked once everything has been read.
// If the given upload could not be found, the error tusd.ErrNotFound should
// be returned.
//func (store EitStore) GetReader(id string) (io.Reader, error) {
//    return os.Open(store.binPath(id))
//}
func (store EitStore) GetReader(id string) (io.Reader, error) {
    tusdFile := store.getTusdFile(id)
    if tusdFile == nil {
        return nil, errors.New("file info not found")
    }

    binPath := fmt.Sprintf("%s/%s.bin", tusdFile.FilePath, id)
    return os.Open(binPath)
}

// ConcatUploads concatenations the content from the provided partial uploads
// and write the result in the destination upload which is specified by its
// ID. The caller (usually the handler) must and will ensure that this
// destination upload has been created before with enough space to hold all
// partial uploads. The order, in which the partial uploads are supplied,
// must be respected during concatenation.
func (store EitStore) ConcatUploads(destination string, partialUploads []string) error {
    tusdFile := tusdFilesDao.FindOneById(destination)
    if tusdFile == nil {
        return errors.New("file info not found")
    }

    binPath := fmt.Sprintf("%s/%s.bin", tusdFile.FilePath, destination)

    file, err := os.OpenFile(binPath, os.O_WRONLY|os.O_APPEND, defaultFilePerm)
    if err != nil {
        return err
    }
    defer file.Close()

    for _, id := range partialUploads {
        src, err := store.GetReader(id)
        if err != nil {
            return err
        }

        if _, err := io.Copy(file, src); err != nil {
            return err
        }
    }

    return err
}

func (store EitStore) DeclareLength(id string, length int64) error {
    info, err := store.GetInfo(id)
    if err != nil {
        return err
    }
    info.Size = length
    info.SizeIsDeferred = false
    return store.writeInfo(id, info)
}

// newLock contructs a new Lockfile instance.
func (store EitStore) newLock(id string) (lockfile.Lockfile, error) {
    path := store.path(id)
    lockPath := fmt.Sprintf("%s/%s", path, id+".lock") //  filepath.Abs(filepath.Join(store.Path, id+".lock"))

    // We use Lockfile directly instead of lockfile.New to bypass the unnecessary
    // check whether the provided path is absolute since we just resolved it
    // on our own.
    return lockfile.Lockfile(lockPath), nil
}

// binPath returns the path to the .bin storing the binary data.
func (store EitStore) binPath(id string) string {
    path := store.path(id)
    return fmt.Sprintf("%s/%s", path, id+".bin")
}

// infoPath returns the path to the .info file storing the file's info.
func (store EitStore) infoPath(id string) string {
    path := store.path(id)
    return fmt.Sprintf("%s/%s", path, id+".info")
}

// writeInfo updates the entire information. Everything will be overwritten.
func (store EitStore) writeInfo(id string, info tusd.FileInfo) error {
    data, err := yaml.Marshal(info)
    if err != nil {
        return err
    }
    return ioutil.WriteFile(store.infoPath(id), data, defaultFilePerm)
}

func (store EitStore) path(id string) string {
    path := filepath.Join(store.Path, id[0:2]+"/", id[2:4]+"/")
    return path
}

func (store EitStore) getTusdFile(id string) *tusdx_model.TusdFilesModel {
    tusdFile := getTusdFileFromRedis(id)
    if tusdFile == nil {
        tusdFile = tusdFilesDao.FindOneById(id)
        if tusdFile == nil {
            return nil
        }
        go setTusdFileToRedis(tusdFile)
    }
    return tusdFile
}
