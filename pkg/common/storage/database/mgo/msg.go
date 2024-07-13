package mgo

import (
	"context"
	"fmt"
	"time"

	"github.com/openimsdk/open-im-server/v3/pkg/common/storage/database"
	"github.com/openimsdk/open-im-server/v3/pkg/common/storage/model"
	"github.com/openimsdk/tools/utils/datautil"

	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/protocol/msg"
	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/db/mongoutil"
	"github.com/openimsdk/tools/errs"
	"github.com/openimsdk/tools/log"
	"github.com/openimsdk/tools/utils/jsonutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func NewMsgMongo(db *mongo.Database) (database.Msg, error) {
	coll := db.Collection(new(model.MsgDocModel).TableName())
	_, err := coll.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys: bson.D{
			{Key: "doc_id", Value: 1},
		},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return &MsgMgo{coll: coll}, nil
}

type MsgMgo struct {
	coll  *mongo.Collection
	model model.MsgDocModel
}

func (m *MsgMgo) PushMsgsToDoc(ctx context.Context, docID string, msgsToMongo []model.MsgInfoModel) error {
	filter := bson.M{"doc_id": docID}
	update := bson.M{"$push": bson.M{"msgs": bson.M{"$each": msgsToMongo}}}
	return mongoutil.UpdateOne(ctx, m.coll, filter, update, false)
}

func (m *MsgMgo) Create(ctx context.Context, msg *model.MsgDocModel) error {
	return mongoutil.InsertMany(ctx, m.coll, []*model.MsgDocModel{msg})
}

func (m *MsgMgo) UpdateMsg(ctx context.Context, docID string, index int64, key string, value any) (*mongo.UpdateResult, error) {
	var field string
	if key == "" {
		field = fmt.Sprintf("msgs.%d", index)
	} else {
		field = fmt.Sprintf("msgs.%d.%s", index, key)
	}
	filter := bson.M{"doc_id": docID}
	update := bson.M{"$set": bson.M{field: value}}
	return mongoutil.UpdateOneResult(ctx, m.coll, filter, update)
}

func (m *MsgMgo) PushUnique(ctx context.Context, docID string, index int64, key string, value any) (*mongo.UpdateResult, error) {
	var field string
	if key == "" {
		field = fmt.Sprintf("msgs.%d", index)
	} else {
		field = fmt.Sprintf("msgs.%d.%s", index, key)
	}
	filter := bson.M{"doc_id": docID}
	update := bson.M{
		"$addToSet": bson.M{
			field: bson.M{"$each": value},
		},
	}
	return mongoutil.UpdateOneResult(ctx, m.coll, filter, update)
}

func (m *MsgMgo) UpdateMsgContent(ctx context.Context, docID string, index int64, msg []byte) error {
	filter := bson.M{"doc_id": docID}
	update := bson.M{"$set": bson.M{fmt.Sprintf("msgs.%d.msg", index): msg}}
	return mongoutil.UpdateOne(ctx, m.coll, filter, update, false)
}

func (m *MsgMgo) IsExistDocID(ctx context.Context, docID string) (bool, error) {
	return mongoutil.Exist(ctx, m.coll, bson.M{"doc_id": docID})
}

func (m *MsgMgo) FindOneByDocID(ctx context.Context, docID string) (*model.MsgDocModel, error) {
	return mongoutil.FindOne[*model.MsgDocModel](ctx, m.coll, bson.M{"doc_id": docID})
}

func (m *MsgMgo) GetMsgBySeqIndexIn1Doc(ctx context.Context, userID, docID string, seqs []int64) ([]*model.MsgInfoModel, error) {
	indexs := make([]int64, 0, len(seqs))
	for _, seq := range seqs {
		indexs = append(indexs, m.model.GetMsgIndex(seq))
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "doc_id", Value: docID},
		}}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "doc_id", Value: 1},
			{Key: "msgs", Value: bson.D{
				{Key: "$map", Value: bson.D{
					{Key: "input", Value: indexs},
					{Key: "as", Value: "index"},
					{Key: "in", Value: bson.D{
						{Key: "$arrayElemAt", Value: bson.A{"$msgs", "$$index"}},
					}},
				}},
			}},
		}}},
	}
	msgDocModel, err := mongoutil.Aggregate[*model.MsgDocModel](ctx, m.coll, pipeline)
	if err != nil {
		return nil, err
	}
	if len(msgDocModel) == 0 {
		return nil, errs.Wrap(mongo.ErrNoDocuments)
	}
	msgs := make([]*model.MsgInfoModel, 0, len(msgDocModel[0].Msg))
	for i := range msgDocModel[0].Msg {
		msg := msgDocModel[0].Msg[i]
		if msg == nil || msg.Msg == nil {
			continue
		}
		if datautil.Contain(userID, msg.DelList...) {
			msg.Msg.Content = ""
			msg.Msg.Status = constant.MsgDeleted
		}
		if msg.Revoke != nil {
			revokeContent := sdkws.MessageRevokedContent{
				RevokerID:                   msg.Revoke.UserID,
				RevokerRole:                 msg.Revoke.Role,
				ClientMsgID:                 msg.Msg.ClientMsgID,
				RevokerNickname:             msg.Revoke.Nickname,
				RevokeTime:                  msg.Revoke.Time,
				SourceMessageSendTime:       msg.Msg.SendTime,
				SourceMessageSendID:         msg.Msg.SendID,
				SourceMessageSenderNickname: msg.Msg.SenderNickname,
				SessionType:                 msg.Msg.SessionType,
				Seq:                         msg.Msg.Seq,
				Ex:                          msg.Msg.Ex,
			}
			data, err := jsonutil.JsonMarshal(&revokeContent)
			if err != nil {
				return nil, errs.WrapMsg(err, fmt.Sprintf("docID is %s, seqs is %v", docID, seqs))
			}
			elem := sdkws.NotificationElem{
				Detail: string(data),
			}
			content, err := jsonutil.JsonMarshal(&elem)
			if err != nil {
				return nil, errs.WrapMsg(err, fmt.Sprintf("docID is %s, seqs is %v", docID, seqs))
			}
			msg.Msg.ContentType = constant.MsgRevokeNotification
			msg.Msg.Content = string(content)
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

func (m *MsgMgo) GetNewestMsg(ctx context.Context, conversationID string) (*model.MsgInfoModel, error) {
	for skip := int64(0); ; skip++ {
		msgDocModel, err := m.GetMsgDocModelByIndex(ctx, conversationID, skip, -1)
		if err != nil {
			return nil, err
		}
		for i := len(msgDocModel.Msg) - 1; i >= 0; i-- {
			if msgDocModel.Msg[i].Msg != nil {
				return msgDocModel.Msg[i], nil
			}
		}
	}
}

func (m *MsgMgo) GetOldestMsg(ctx context.Context, conversationID string) (*model.MsgInfoModel, error) {
	for skip := int64(0); ; skip++ {
		msgDocModel, err := m.GetMsgDocModelByIndex(ctx, conversationID, skip, 1)
		if err != nil {
			return nil, err
		}
		for i, v := range msgDocModel.Msg {
			if v.Msg != nil {
				return msgDocModel.Msg[i], nil
			}
		}
	}
}

func (m *MsgMgo) DeleteDocs(ctx context.Context, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	return mongoutil.DeleteMany(ctx, m.coll, bson.M{"doc_id": bson.M{"$in": docIDs}})
}

func (m *MsgMgo) GetMsgDocModelByIndex(ctx context.Context, conversationID string, index, sort int64) (*model.MsgDocModel, error) {
	if sort != 1 && sort != -1 {
		return nil, errs.ErrArgs.WrapMsg("mongo sort must be 1 or -1")
	}
	opt := options.Find().SetLimit(1).SetSkip(index).SetSort(bson.M{"doc_id": sort}).SetLimit(1)
	filter := bson.M{"doc_id": primitive.Regex{Pattern: fmt.Sprintf("^%s:", conversationID)}}
	msgs, err := mongoutil.Find[*model.MsgDocModel](ctx, m.coll, filter, opt)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		return msgs[0], nil
	}
	return nil, errs.Wrap(model.ErrMsgListNotExist)
}

func (m *MsgMgo) DeleteMsgsInOneDocByIndex(ctx context.Context, docID string, indexes []int) error {
	update := bson.M{
		"$set": bson.M{},
	}
	for _, index := range indexes {
		update["$set"].(bson.M)[fmt.Sprintf("msgs.%d", index)] = bson.M{
			"msg": nil,
		}
	}
	_, err := mongoutil.UpdateMany(ctx, m.coll, bson.M{"doc_id": docID}, update)
	return err
}

func (m *MsgMgo) MarkSingleChatMsgsAsRead(ctx context.Context, userID string, docID string, indexes []int64) error {
	var updates []mongo.WriteModel
	for _, index := range indexes {
		filter := bson.M{
			"doc_id": docID,
			fmt.Sprintf("msgs.%d.msg.send_id", index): bson.M{
				"$ne": userID,
			},
		}
		update := bson.M{
			"$set": bson.M{
				fmt.Sprintf("msgs.%d.is_read", index): true,
			},
		}
		updateModel := mongo.NewUpdateManyModel().
			SetFilter(filter).
			SetUpdate(update)
		updates = append(updates, updateModel)
	}
	if _, err := m.coll.BulkWrite(ctx, updates); err != nil {
		return errs.WrapMsg(err, fmt.Sprintf("docID is %s, indexes is %v", docID, indexes))
	}
	return nil
}

func (m *MsgMgo) SearchMessage(ctx context.Context, req *msg.SearchMessageReq) (int32, []*model.MsgInfoModel, error) {
	where := make(bson.A, 0, 6)
	if req.RecvID != "" {
		where = append(where, bson.M{"msgs.msg.recv_id": req.RecvID})
	}
	if req.SendID != "" {
		where = append(where, bson.M{"msgs.msg.send_id": req.SendID})
	}
	if req.ContentType != 0 {
		where = append(where, bson.M{"msgs.msg.content_type": req.ContentType})
	}
	if req.SessionType != 0 {
		where = append(where, bson.M{"msgs.msg.session_type": req.SessionType})
	}
	if req.SendTime != "" {
		sendTime, err := time.Parse(time.DateOnly, req.SendTime)
		if err != nil {
			return 0, nil, errs.ErrArgs.WrapMsg("invalid sendTime", "req", req.SendTime, "format", time.DateOnly, "cause", err.Error())
		}
		where = append(where,
			bson.M{
				"msgs.msg.send_time": bson.M{
					"$gte": sendTime.UnixMilli(),
				},
			},
			bson.M{
				"msgs.msg.send_time": bson.M{
					"$lt": sendTime.Add(time.Hour * 24).UnixMilli(),
				},
			},
		)
	}
	pipeline := bson.A{
		bson.M{
			"$unwind": "$msgs",
		},
	}
	if len(where) > 0 {
		pipeline = append(pipeline, bson.M{
			"$match": bson.M{"$and": where},
		})
	}
	pipeline = append(pipeline,
		bson.M{
			"$project": bson.M{
				"_id": 0,
				"msg": "$msgs.msg",
			},
		},
		bson.M{
			"$count": "count",
		},
	)
	count, err := mongoutil.Aggregate[int32](ctx, m.coll, pipeline)
	if err != nil {
		return 0, nil, err
	}
	if len(count) == 0 || count[0] == 0 {
		return 0, nil, nil
	}
	pipeline = pipeline[:len(pipeline)-1]
	pipeline = append(pipeline,
		bson.M{
			"$skip": (req.Pagination.GetPageNumber() - 1) * req.Pagination.GetShowNumber(),
		},
		bson.M{
			"$limit": req.Pagination.GetShowNumber(),
		},
	)
	msgs, err := mongoutil.Aggregate[*model.MsgInfoModel](ctx, m.coll, pipeline)
	if err != nil {
		return 0, nil, err
	}
	for i := range msgs {
		msgInfo := msgs[i]
		if msgInfo == nil || msgInfo.Msg == nil {
			continue
		}
		if msgInfo.Revoke != nil {
			revokeContent := sdkws.MessageRevokedContent{
				RevokerID:                   msgInfo.Revoke.UserID,
				RevokerRole:                 msgInfo.Revoke.Role,
				ClientMsgID:                 msgInfo.Msg.ClientMsgID,
				RevokerNickname:             msgInfo.Revoke.Nickname,
				RevokeTime:                  msgInfo.Revoke.Time,
				SourceMessageSendTime:       msgInfo.Msg.SendTime,
				SourceMessageSendID:         msgInfo.Msg.SendID,
				SourceMessageSenderNickname: msgInfo.Msg.SenderNickname,
				SessionType:                 msgInfo.Msg.SessionType,
				Seq:                         msgInfo.Msg.Seq,
				Ex:                          msgInfo.Msg.Ex,
			}
			data, err := jsonutil.JsonMarshal(&revokeContent)
			if err != nil {
				return 0, nil, errs.WrapMsg(err, "json.Marshal revokeContent")
			}
			elem := sdkws.NotificationElem{Detail: string(data)}
			content, err := jsonutil.JsonMarshal(&elem)
			if err != nil {
				return 0, nil, errs.WrapMsg(err, "json.Marshal elem")
			}
			msgInfo.Msg.ContentType = constant.MsgRevokeNotification
			msgInfo.Msg.Content = string(content)
		}
	}
	//start := (req.Pagination.PageNumber - 1) * req.Pagination.ShowNumber
	//n := int32(len(msgs))
	//if start >= n {
	//	return n, []*relation.MsgInfoModel{}, nil
	//}
	//if start+req.Pagination.ShowNumber < n {
	//	msgs = msgs[start : start+req.Pagination.ShowNumber]
	//} else {
	//	msgs = msgs[start:]
	//}
	return count[0], msgs, nil
}

func (m *MsgMgo) RangeUserSendCount(ctx context.Context, start time.Time, end time.Time, group bool, ase bool, pageNumber int32, showNumber int32) (msgCount int64, userCount int64, users []*model.UserCount, dateCount map[string]int64, err error) {
	var sort int
	if ase {
		sort = 1
	} else {
		sort = -1
	}
	type Result struct {
		MsgCount  int64 `bson:"msg_count"`
		UserCount int64 `bson:"user_count"`
		Users     []struct {
			UserID string `bson:"_id"`
			Count  int64  `bson:"count"`
		} `bson:"users"`
		Dates []struct {
			Date  string `bson:"_id"`
			Count int64  `bson:"count"`
		} `bson:"dates"`
	}
	or := bson.A{
		bson.M{
			"doc_id": bson.M{
				"$regex":   "^si_",
				"$options": "i",
			},
		},
	}
	if group {
		or = append(or,
			bson.M{
				"doc_id": bson.M{
					"$regex":   "^g_",
					"$options": "i",
				},
			},
			bson.M{
				"doc_id": bson.M{
					"$regex":   "^sg_",
					"$options": "i",
				},
			},
		)
	}
	pipeline := bson.A{
		bson.M{
			"$match": bson.M{
				"$and": bson.A{
					bson.M{
						"msgs.msg.send_time": bson.M{
							"$gte": start.UnixMilli(),
							"$lt":  end.UnixMilli(),
						},
					},
					bson.M{
						"$or": or,
					},
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"msgs": bson.M{
					"$filter": bson.M{
						"input": "$msgs",
						"as":    "item",
						"cond": bson.M{
							"$and": bson.A{
								bson.M{
									"$gte": bson.A{
										"$$item.msg.send_time", start.UnixMilli(),
									},
								},
								bson.M{
									"$lt": bson.A{
										"$$item.msg.send_time", end.UnixMilli(),
									},
								},
							},
						},
					},
				},
			},
		},
		bson.M{
			"$project": bson.M{
				"_id": 0,
			},
		},
		bson.M{
			"$project": bson.M{
				"result": bson.M{
					"$map": bson.M{
						"input": "$msgs",
						"as":    "item",
						"in": bson.M{
							"user_id": "$$item.msg.send_id",
							"send_date": bson.M{
								"$dateToString": bson.M{
									"format": "%Y-%m-%d",
									"date": bson.M{
										"$toDate": "$$item.msg.send_time", // Millisecond timestamp
									},
								},
							},
						},
					},
				},
			},
		},
		bson.M{
			"$unwind": "$result",
		},
		bson.M{
			"$group": bson.M{
				"_id": "$result.send_date",
				"count": bson.M{
					"$sum": 1,
				},
				"original": bson.M{
					"$push": "$$ROOT",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": "$$ROOT",
			},
		},
		bson.M{
			"$project": bson.M{
				"_id":            0,
				"count":          0,
				"dates.original": 0,
			},
		},
		bson.M{
			"$group": bson.M{
				"_id": nil,
				"count": bson.M{
					"$sum": 1,
				},
				"dates": bson.M{
					"$push": "$dates",
				},
				"original": bson.M{
					"$push": "$original",
				},
			},
		},
		bson.M{
			"$unwind": "$original",
		},
		bson.M{
			"$unwind": "$original",
		},
		bson.M{
			"$group": bson.M{
				"_id": "$original.result.user_id",
				"count": bson.M{
					"$sum": 1,
				},
				"original": bson.M{
					"$push": "$dates",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": bson.M{
					"$arrayElemAt": bson.A{"$original", 0},
				},
			},
		},
		bson.M{
			"$project": bson.M{
				"original": 0,
			},
		},
		bson.M{
			"$sort": bson.M{
				"count": sort,
			},
		},
		bson.M{
			"$group": bson.M{
				"_id": nil,
				"user_count": bson.M{
					"$sum": 1,
				},
				"users": bson.M{
					"$push": "$$ROOT",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": bson.M{
					"$arrayElemAt": bson.A{"$users", 0},
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": "$dates.dates",
			},
		},
		bson.M{
			"$project": bson.M{
				"_id":         0,
				"users.dates": 0,
			},
		},
		bson.M{
			"$addFields": bson.M{
				"msg_count": bson.M{
					"$sum": "$users.count",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"users": bson.M{
					"$slice": bson.A{"$users", pageNumber - 1, showNumber},
				},
			},
		},
	}
	result, err := mongoutil.Aggregate[*Result](ctx, m.coll, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(result) == 0 {
		return 0, 0, nil, nil, errs.Wrap(err)
	}
	users = make([]*model.UserCount, len(result[0].Users))
	for i, r := range result[0].Users {
		users[i] = &model.UserCount{
			UserID: r.UserID,
			Count:  r.Count,
		}
	}
	dateCount = make(map[string]int64)
	for _, r := range result[0].Dates {
		dateCount[r.Date] = r.Count
	}
	return result[0].MsgCount, result[0].UserCount, users, dateCount, nil
}

func (m *MsgMgo) RangeGroupSendCount(ctx context.Context, start time.Time, end time.Time, ase bool, pageNumber int32, showNumber int32) (msgCount int64, userCount int64, groups []*model.GroupCount, dateCount map[string]int64, err error) {
	var sort int
	if ase {
		sort = 1
	} else {
		sort = -1
	}
	type Result struct {
		MsgCount  int64 `bson:"msg_count"`
		UserCount int64 `bson:"user_count"`
		Groups    []struct {
			GroupID string `bson:"_id"`
			Count   int64  `bson:"count"`
		} `bson:"groups"`
		Dates []struct {
			Date  string `bson:"_id"`
			Count int64  `bson:"count"`
		} `bson:"dates"`
	}
	pipeline := bson.A{
		bson.M{
			"$match": bson.M{
				"$and": bson.A{
					bson.M{
						"msgs.msg.send_time": bson.M{
							"$gte": start.UnixMilli(),
							"$lt":  end.UnixMilli(),
						},
					},
					bson.M{
						"$or": bson.A{
							bson.M{
								"doc_id": bson.M{
									"$regex":   "^g_",
									"$options": "i",
								},
							},
							bson.M{
								"doc_id": bson.M{
									"$regex":   "^sg_",
									"$options": "i",
								},
							},
						},
					},
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"msgs": bson.M{
					"$filter": bson.M{
						"input": "$msgs",
						"as":    "item",
						"cond": bson.M{
							"$and": bson.A{
								bson.M{
									"$gte": bson.A{
										"$$item.msg.send_time", start.UnixMilli(),
									},
								},
								bson.M{
									"$lt": bson.A{
										"$$item.msg.send_time", end.UnixMilli(),
									},
								},
							},
						},
					},
				},
			},
		},
		bson.M{
			"$project": bson.M{
				"_id": 0,
			},
		},
		bson.M{
			"$project": bson.M{
				"result": bson.M{
					"$map": bson.M{
						"input": "$msgs",
						"as":    "item",
						"in": bson.M{
							"group_id": "$$item.msg.group_id",
							"send_date": bson.M{
								"$dateToString": bson.M{
									"format": "%Y-%m-%d",
									"date": bson.M{
										"$toDate": "$$item.msg.send_time", // Millisecond timestamp
									},
								},
							},
						},
					},
				},
			},
		},
		bson.M{
			"$unwind": "$result",
		},
		bson.M{
			"$group": bson.M{
				"_id": "$result.send_date",
				"count": bson.M{
					"$sum": 1,
				},
				"original": bson.M{
					"$push": "$$ROOT",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": "$$ROOT",
			},
		},
		bson.M{
			"$project": bson.M{
				"_id":            0,
				"count":          0,
				"dates.original": 0,
			},
		},
		bson.M{
			"$group": bson.M{
				"_id": nil,
				"count": bson.M{
					"$sum": 1,
				},
				"dates": bson.M{
					"$push": "$dates",
				},
				"original": bson.M{
					"$push": "$original",
				},
			},
		},
		bson.M{
			"$unwind": "$original",
		},
		bson.M{
			"$unwind": "$original",
		},
		bson.M{
			"$group": bson.M{
				"_id": "$original.result.group_id",
				"count": bson.M{
					"$sum": 1,
				},
				"original": bson.M{
					"$push": "$dates",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": bson.M{
					"$arrayElemAt": bson.A{"$original", 0},
				},
			},
		},
		bson.M{
			"$project": bson.M{
				"original": 0,
			},
		},
		bson.M{
			"$sort": bson.M{
				"count": sort,
			},
		},
		bson.M{
			"$group": bson.M{
				"_id": nil,
				"user_count": bson.M{
					"$sum": 1,
				},
				"groups": bson.M{
					"$push": "$$ROOT",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": bson.M{
					"$arrayElemAt": bson.A{"$groups", 0},
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"dates": "$dates.dates",
			},
		},
		bson.M{
			"$project": bson.M{
				"_id":          0,
				"groups.dates": 0,
			},
		},
		bson.M{
			"$addFields": bson.M{
				"msg_count": bson.M{
					"$sum": "$groups.count",
				},
			},
		},
		bson.M{
			"$addFields": bson.M{
				"groups": bson.M{
					"$slice": bson.A{"$groups", pageNumber - 1, showNumber},
				},
			},
		},
	}
	result, err := mongoutil.Aggregate[*Result](ctx, m.coll, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(result) == 0 {
		return 0, 0, nil, nil, errs.Wrap(err)
	}
	groups = make([]*model.GroupCount, len(result[0].Groups))
	for i, r := range result[0].Groups {
		groups[i] = &model.GroupCount{
			GroupID: r.GroupID,
			Count:   r.Count,
		}
	}
	dateCount = make(map[string]int64)
	for _, r := range result[0].Dates {
		dateCount[r.Date] = r.Count
	}
	return result[0].MsgCount, result[0].UserCount, groups, dateCount, nil
}

func (m *MsgMgo) ConvertMsgsDocLen(ctx context.Context, conversationIDs []string) {
	for _, conversationID := range conversationIDs {
		regex := primitive.Regex{Pattern: fmt.Sprintf("^%s:", conversationID)}
		msgDocs, err := mongoutil.Find[*model.MsgDocModel](ctx, m.coll, bson.M{"doc_id": regex})
		if err != nil {
			log.ZError(ctx, "convertAll find msg doc failed", err, "conversationID", conversationID)
			continue
		}
		if len(msgDocs) < 1 {
			continue
		}
		log.ZDebug(ctx, "msg doc convert", "conversationID", conversationID, "len(msgDocs)", len(msgDocs))
		if len(msgDocs[0].Msg) == int(m.model.GetSingleGocMsgNum5000()) {
			if err := mongoutil.DeleteMany(ctx, m.coll, bson.M{"doc_id": regex}); err != nil {
				log.ZError(ctx, "convertAll delete many failed", err, "conversationID", conversationID)
				continue
			}
			var newMsgDocs []any
			for _, msgDoc := range msgDocs {
				if int64(len(msgDoc.Msg)) == m.model.GetSingleGocMsgNum() {
					continue
				}
				var index int64
				for index < int64(len(msgDoc.Msg)) {
					msg := msgDoc.Msg[index]
					if msg != nil && msg.Msg != nil {
						msgDocModel := model.MsgDocModel{DocID: m.model.GetDocID(conversationID, msg.Msg.Seq)}
						end := index + m.model.GetSingleGocMsgNum()
						if int(end) >= len(msgDoc.Msg) {
							msgDocModel.Msg = msgDoc.Msg[index:]
						} else {
							msgDocModel.Msg = msgDoc.Msg[index:end]
						}
						newMsgDocs = append(newMsgDocs, msgDocModel)
						index = end
					} else {
						break
					}
				}
			}
			if err = mongoutil.InsertMany(ctx, m.coll, newMsgDocs); err != nil {
				log.ZError(ctx, "convertAll insert many failed", err, "conversationID", conversationID, "len(newMsgDocs)", len(newMsgDocs))
			} else {
				log.ZDebug(ctx, "msg doc convert", "conversationID", conversationID, "len(newMsgDocs)", len(newMsgDocs))
			}
		}
	}
}

func (m *MsgMgo) GetBeforeMsg(ctx context.Context, ts int64, limit int) ([]*model.MsgDocModel, error) {
	return mongoutil.Aggregate[*model.MsgDocModel](ctx, m.coll, []bson.M{
		{
			"$match": bson.M{
				"msgs.msg.send_time": bson.M{
					"$lt": ts,
				},
			},
		},
		{
			"$project": bson.M{
				"_id":                0,
				"doc_id":             1,
				"msgs.msg.send_time": 1,
				"msgs.msg.seq":       1,
			},
		},
		{
			"$limit": limit,
		},
	})
}

func (m *MsgMgo) DeleteMsgByIndex(ctx context.Context, docID string, index []int) error {
	if len(index) == 0 {
		return nil
	}
	model := &model.MsgInfoModel{DelList: []string{}}
	set := make(map[string]any)
	for i := range index {
		set[fmt.Sprintf("msgs.%d", i)] = model
	}
	return mongoutil.UpdateOne(ctx, m.coll, bson.M{"doc_id": docID}, bson.M{"$set": set}, true)
}

//func (m *MsgMgo) ClearMsg(ctx context.Context, t time.Time) (int64, error) {
//	ts := t.UnixMilli()
//	var count int64
//	for {
//		msgs, err := m.GetBeforeMsg(ctx, ts, 100)
//		if err != nil {
//			return count, err
//		}
//		if len(msgs) == 0 {
//			return count, nil
//		}
//		for _, msg := range msgs {
//			num, err := m.deleteOneMsg(ctx, ts, msg)
//			count += num
//			if err != nil {
//				return count, err
//			}
//		}
//	}
//}

func (m *MsgMgo) DeleteDoc(ctx context.Context, docID string) error {
	return mongoutil.DeleteOne(ctx, m.coll, bson.M{"doc_id": docID})
}

//func (m *MsgMgo) DeleteDocMsg(ctx context.Context, ts int64, doc *relation.MsgDocModel) (int64, error) {
//	var notNull int
//	index := make([]int, 0, len(doc.Msg))
//	for i, message := range doc.Msg {
//		if message.Msg != nil {
//			notNull++
//			if message.Msg.SendTime < ts {
//				index = append(index, i)
//			}
//		}
//	}
//	if len(index) == 0 {
//		return 0, errs.New("no msg to delete").WrapMsg("deleteOneMsg", "docID", doc.DocID)
//	}
//	if len(index) == notNull {
//		if err := m.DeleteDoc(ctx, doc.DocID); err != nil {
//			return 0, err
//		}
//	} else {
//		if err := m.setNullMsg(ctx, doc.DocID, index); err != nil {
//			return 0, err
//		}
//	}
//	return int64(len(index)), nil
//}