package tusdx

import (
    "github.com/MaxTech/tusdx/tusdx_model"
    "github.com/go-xorm/xorm"
)

type tusdFilesObject struct {
}

var tusdFilesDao *tusdFilesObject

func (wfd *tusdFilesObject) TableName() string {
    return "tusd_files"
}

func (wfd *tusdFilesObject) getSessionByQueryMap(queryMap map[string]interface{}) *xorm.Session {
    return mySQLEngine.Table(wfd.TableName()).Where(queryMap)
}

func (wfd *tusdFilesObject) FindOneById(id string) *tusdx_model.TusdFilesModel {
    session := wfd.getSessionByQueryMap(map[string]interface{}{
        "id": id,
    })

    tusdFileModel := new(tusdx_model.TusdFilesModel)

    has, err := session.Get(tusdFileModel)
    if err != nil {
        logger.Error(err)
    }

    if !has {
        return nil
    }
    return tusdFileModel
}

func (wfd *tusdFilesObject) InsertOneByModel(model *tusdx_model.TusdFilesModel) int64 {
    session := mySQLEngine.Table(wfd.TableName())

    insNum, err := session.InsertOne(model)
    if err != nil {
        logger.Error(err)
        return -1
    }

    return insNum
}

func (wfd *tusdFilesObject) UpdateOneByModel(model *tusdx_model.TusdFilesModel) int64 {
    session := mySQLEngine.Table(wfd.TableName()).ID(model.Id)

    updNum, err := session.Update(model)
    if err != nil {
        logger.Error(err)
        return -1
    }

    return updNum
}

func (wfd *tusdFilesObject) DeleteOneByModel(model *tusdx_model.TusdFilesModel) int64 {
    session := mySQLEngine.Table(wfd.TableName()).ID(model.Id)

    model = new(tusdx_model.TusdFilesModel)
    delNum, err := session.Delete(model)
    if err != nil {
        logger.Error(err)
        return -1
    }

    return delNum
}
