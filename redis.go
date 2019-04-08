package tusdx

import (
    "encoding/json"
    "fmt"
    "github.com/gomodule/redigo/redis"
    "github.com/maxtech/tusdx/tusdx_model"
)

var (
    redisPool *redis.Pool
    prefix    = "tusd"
)

func getTusdFileFromRedis(id string) *tusdx_model.TusdFilesModel {
    var tusdFile *tusdx_model.TusdFilesModel = nil

    conn := redisPool.Get()
    defer conn.Close()

    jsonStr, _ := redis.String(conn.Do("get", makeRedisKey(id)))

    err := json.Unmarshal([]byte(jsonStr), &tusdFile)
    if err != nil {
        logger.Error(err)
        tusdFile = nil
    }
    return tusdFile
}

func setTusdFileToRedis(tusdFile *tusdx_model.TusdFilesModel) {
    conn := redisPool.Get()
    defer conn.Close()
    jsonB, _ := json.Marshal(tusdFile)
    _, _ = conn.Do("set", makeRedisKey(tusdFile.Id), string(jsonB))
    return
}

func makeRedisKey(str string) string {
    return fmt.Sprintf("%s:%s", prefix, str)
}
