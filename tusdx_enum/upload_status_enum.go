package tusdx_enum

type UploadStatusEnum int

const (
    UploadCreated   UploadStatusEnum = 0
    UploadPatching  UploadStatusEnum = 1
    UploadCompleted UploadStatusEnum = 2
)

var uploadStatusMap = map[UploadStatusEnum]string{
    UploadCreated:   "上传文件已创建",
    UploadPatching:  "上传续传中",
    UploadCompleted: "上传完成",
}

func (use UploadStatusEnum) Text() string {
    return uploadStatusMap[use]
}
