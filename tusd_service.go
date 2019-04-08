package tusdx

import (
    "fmt"
    "io/ioutil"
)

type tusdService struct {
}

var TusdService *tusdService

func (ts *tusdService) GetFileBytesById(id string) []byte {
    tusdFile := tusdFilesDao.FindOneById(id)
    if tusdFile == nil {
        return nil
    }
    fileName := fmt.Sprintf("%s.bin", id)
    filePath := fmt.Sprintf("%s/%s", tusdFile.FilePath, fileName)
    fileBytes, err := ioutil.ReadFile(filePath)

    if err != nil {
        logger.Error(err)
        return nil
    }
    return fileBytes
}
