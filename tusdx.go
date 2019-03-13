package tusdx

import (
    "github.com/MaxTech/log"
    "github.com/go-xorm/xorm"
    "github.com/gomodule/redigo/redis"
)

func Init(mysqlDataSource *xorm.Engine, initRedisPool *redis.Pool, initLogger log.AppLogger) {
    mySQLEngine = mysqlDataSource
    redisPool = initRedisPool
    logger = initLogger
}
