package tusdx_model

import "time"

type TusdFilesModel struct {
    Id           string    `json:"id" xorm:"'id' pk"`
    FileName     string    `json:"file_name" xorm:"'file_name'"`
    FilePath     string    `json:"file_path" xorm:"'file_path'"`
    UploadStatus int       `json:"upload_status" xorm:"'upload_status'"`
    CreateTime   time.Time `json:"create_time" xorm:"'create_time'"`
}
