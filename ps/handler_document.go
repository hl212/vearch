// Copyright 2019 The Vearch Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package ps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/vearch/vearch/util/cbbytes"

	"github.com/smallnest/rpcx/share"
	"github.com/spf13/cast"
	"github.com/vearch/vearch/client"
	"github.com/vearch/vearch/config"
	"github.com/vearch/vearch/ps/engine/gamma"
	"github.com/vearch/vearch/ps/engine/mapping"

	"github.com/vearch/vearch/proto/entity"
	"github.com/vearch/vearch/proto/vearchpb"
	"github.com/vearch/vearch/util/log"
	"github.com/vearch/vearch/util/server/rpc/handler"
	"go.uber.org/atomic"
)

type limitPlugin struct {
	size  int64
	limit *atomic.Int64
}

func (lp *limitPlugin) HandleConnAccept(conn net.Conn) (net.Conn, bool) {
	if lp.limit.Load() > lp.size {

		for _, m := range config.Conf().Masters {
			if m.Address == conn.RemoteAddr().Network() || strings.HasPrefix(conn.RemoteAddr().Network(), m.Address+":") {
				log.Info("too many routine:[%d]  but this conn is master so can conn")
				return conn, true
			}
		}

		log.Warn("too many routine:[%d] for limt so skip %s conn", lp.limit.Load(), conn.RemoteAddr().String())
		return conn, false
	}
	return conn, true
}

func ExportToRpcHandler(server *Server) {

	initHandler := &InitHandler{server: server}
	psErrorChange := psErrorChange(server)

	limitPlugin := &limitPlugin{limit: atomic.NewInt64(0), size: 50000}
	server.rpcServer.AddPlugin(limitPlugin)

	if err := server.rpcServer.RegisterName(handler.NewChain(client.UnaryHandler, handler.DefaultPanicHandler, psErrorChange, initHandler, &UnaryHandler{server: server}), ""); err != nil {
		panic(err)
	}

}

type InitHandler struct {
	server *Server
}

func (i *InitHandler) Execute(ctx context.Context, req *vearchpb.PartitionData, reply *vearchpb.PartitionData) error {
	if i.server.stopping.Get() {
		return vearchpb.NewError(vearchpb.ErrorEnum_SERVICE_UNAVAILABLE, nil)
	}

	return nil
}

type UnaryHandler struct {
	server *Server
}

func cost(s string, t time.Time) {
	engTime := time.Now()
	log.Info("%s cost: [%v]", s, engTime.Sub(t))
}

func (handler *UnaryHandler) Execute(ctx context.Context, req *vearchpb.PartitionData, reply *vearchpb.PartitionData) (err error) {
	defer cost("UnaryHandler", time.Now())
	reqMap := ctx.Value(share.ReqMetaDataKey).(map[string]string)
	if spanCtx, err := opentracing.GlobalTracer().Extract(opentracing.TextMap, opentracing.TextMapCarrier(reqMap)); err == nil {
		span := opentracing.StartSpan("server-execute", ext.RPCServerOption(spanCtx))
		defer span.Finish()
	}
	timeout := handler.server.rpcTimeOut * 1000
	if s, ok := reqMap[string(entity.RPC_TIME_OUT)]; ok {
		if t, ok := strconv.Atoi(s); ok == nil {
			if t > 0 {
				timeout = t
			}
		}
	}
	delayTime := time.Duration(timeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, delayTime)
	defer cancel()
	stopCh := make(chan struct{})

	go func(ctx context.Context, req *vearchpb.PartitionData) {
		handler.execute(ctx, req)
		close(stopCh)
	}(ctx, req)
	select {
	case <-stopCh:
		reply.PartitionID = req.PartitionID
		reply.MessageID = req.MessageID
		reply.Items = req.Items
		// reply.SearchRequest = req.SearchRequest
		reply.SearchResponse = req.SearchResponse
		// reply.SearchRequests = req.SearchRequests
		reply.SearchResponses = req.SearchResponses
		reply.DelByQueryResponse = req.DelByQueryResponse
		reply.Err = req.Err
		return
	case <-time.After(delayTime):
		reply.PartitionID = req.PartitionID
		reply.MessageID = req.MessageID
		reply.Items = req.Items
		msg := fmt.Sprintf("This request processing timed out[%dms]", timeout)
		reply.Err = vearchpb.NewError(vearchpb.ErrorEnum_TIMEOUT, errors.New(msg)).GetError()
		log.Error(msg)
		return
	}
}

func (handler *UnaryHandler) execute(ctx context.Context, req *vearchpb.PartitionData) {
	defer func() {
		if r := recover(); r != nil {
			err := vearchpb.NewError(vearchpb.ErrorEnum_RECOVER, errors.New(cast.ToString(r)))
			req.Err = err.GetError()
			log.Error(err.Error())
		}
	}()

	handler.server.concurrent <- true
	defer func() {
		<-handler.server.concurrent
	}()
	select {
	case <-ctx.Done():
		// if this context is timeout, return immediately
		msg := fmt.Sprintf("This request waitting timed out, the server can only deal [%d] request at same time.", handler.server.concurrentNum)
		log.Error(msg)
		return
	default:
		if handler.server == nil {
			log.Info("%s", "ps server is nil")
		}
		store := handler.server.GetPartition(req.PartitionID)
		if store == nil {
			msg := fmt.Sprintf("partition not found, partitionId:[%d]", req.PartitionID)
			log.Error(msg)
			req.Err = vearchpb.NewError(vearchpb.ErrorEnum_PARTITION_NOT_EXIST, errors.New(msg)).GetError()
			return
		}
		var method string
		reqMap := ctx.Value(share.ReqMetaDataKey).(map[string]string)
		method, ok := reqMap[client.HandlerType]
		if !ok {
			err := fmt.Errorf("client type not found in matadata, key [%s]", client.HandlerType)
			req.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
			return
		}
		switch method {
		case client.GetDocsHandler:
			getDocuments(ctx, store, req.Items)
		case client.DeleteDocsHandler:
			deleteDocs(ctx, store, req.Items)
		case client.ReplaceDocHandler:
			update(ctx, store, req.Items)
		case client.BatchHandler:
			bulk(ctx, store, req.Items)
		case client.SearchHandler:
			if req.SearchResponse == nil {
				req.SearchResponse = &vearchpb.SearchResponse{}
			}
			search(ctx, store, req.SearchRequest, req.SearchResponse)
		case client.BulkSearchHandler:
			if req.SearchResponses == nil || len(req.SearchResponses) == 0 {
				searchResps := make([]*vearchpb.SearchResponse, 0)
				for i := 0; i < len(req.SearchRequests); i++ {
					searchReq := req.SearchRequests[i]
					sortFieldMap := searchReq.SortFieldMap
					topSize := searchReq.TopN
					resp := &vearchpb.SearchResponse{SortFieldMap: sortFieldMap, TopSize: topSize}
					searchResps = append(searchResps, resp)
				}
				req.SearchResponses = searchResps
			}
			bulkSearch(ctx, store, req.SearchRequests, req.SearchResponses)
		case client.ForceMergeHandler:
			forceMerge(ctx, store, req.Err)
		case client.DeleteByQueryHandler:
			if req.DelByQueryResponse == nil {
				req.DelByQueryResponse = &vearchpb.DelByQueryeResponse{DelNum: 0}
			}
			deleteByQuery(ctx, store, req.SearchRequest, req.DelByQueryResponse)
		case client.FlushHandler:
			flush(ctx, store, req.Err)
		case client.DeleteByQueryFilterHandler:
			if req.SearchResponse == nil {
				req.SearchResponse = &vearchpb.SearchResponse{}
			}
			deleteByQueryFilter(ctx, store, req.SearchRequest, req.SearchResponse)
		default:
			log.Error("method not found, method: [%s]", method)
			req.Err = vearchpb.NewError(vearchpb.ErrorEnum_METHOD_NOT_IMPLEMENT, nil).GetError()
			return
		}
	}
	return
}

func getDocuments(ctx context.Context, store PartitionStore, items []*vearchpb.Item) {
	for _, item := range items {
		if e := store.GetDocument(ctx, true, item.Doc); e != nil {
			msg := fmt.Sprintf("GetDocument failed, key: [%s], err: [%s]", item.Doc.PKey, e.Error())
			log.Error("%s", msg)
			if vearchErr, ok := e.(*vearchpb.VearchErr); ok {
				item.Err = vearchErr.GetError()
			} else {
				item.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: msg}
			}
		}
	}
}

func deleteDocs(ctx context.Context, store PartitionStore, items []*vearchpb.Item) {
	wg := sync.WaitGroup{}
	for _, item := range items {
		wg.Add(1)
		go func(item *vearchpb.Item) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					item.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: cast.ToString(r)}
				}
			}()
			if len(item.Doc.Fields) != 1 {
				msg := fmt.Sprintf("fileds of doc can only have one field--[%s] when delete", mapping.IdField)
				item.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: msg}
				return
			}
			dataBytes := item.Doc.Fields[0].Value
			docCmd := &vearchpb.DocCmd{Type: vearchpb.OpType_DELETE, Doc: dataBytes}
			if err := store.Write(ctx, docCmd, nil, nil); err != nil {
				log.Error("delete doc failed, err: [%s]", err.Error())
				item.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
			}
		}(item)
	}
	wg.Wait()

}

func bulk(ctx context.Context, store PartitionStore, items []*vearchpb.Item) {
	wg := sync.WaitGroup{}
	gammaArray := make([][]byte, len(items))
	for i, item := range items {
		wg.Add(1)
		go func(item *vearchpb.Item, n int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					item.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: cast.ToString(r)}
				}
			}()
			docGamma := &gamma.Doc{Fields: item.Doc.Fields}
			docBytes := docGamma.Serialize()
			gammaArray[n] = docBytes
			item.Doc.Fields = nil
			item.Err = vearchpb.NewError(vearchpb.ErrorEnum_SUCCESS, nil).GetError()
		}(item, i)
	}
	wg.Wait()
	docCmd := &vearchpb.DocCmd{Type: vearchpb.OpType_BULK, Docs: gammaArray}

	err := store.Write(ctx, docCmd, nil, nil)
	vErr := vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err)
	if vErr.GetError().Code != vearchpb.ErrorEnum_SUCCESS {
		log.Error("Add doc failed, err: [%s]", err.Error())
		for _, item := range items {
			item.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
		}
	} else {
		msgs := strings.Split(vErr.GetError().Msg, ",")
		for i, msg := range msgs {
			if code, _ := strconv.Atoi(msg); code == 0 {
				// log.Debugf("add doc success, %s", msg)
			} else {
				items[i].Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, errors.New(msg)).GetError()
			}
		}
	}
}

func update(ctx context.Context, store PartitionStore, items []*vearchpb.Item) {
	item := items[0]
	docGamma := &gamma.Doc{Fields: item.Doc.Fields}
	docBytes := docGamma.Serialize()
	docCmd := &vearchpb.DocCmd{Type: vearchpb.OpType_REPLACE, Doc: docBytes}
	if err := store.Write(ctx, docCmd, nil, nil); err != nil {
		log.Error("Add doc failed, err: [%s]", err.Error())
		item.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
	} else {
		item.Doc.Fields = nil
	}
}

func search(ctx context.Context, store PartitionStore, request *vearchpb.SearchRequest, response *vearchpb.SearchResponse) {
	startTime := time.Now()
	if err := store.Search(ctx, request, response); err != nil {
		log.Error("search doc failed, err: [%s]", err.Error())
		response.Head.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
	}
	handlerCostTime := (time.Now().Sub(startTime).Seconds()) * 1000
	handlerCostTimeStr := strconv.FormatFloat(handlerCostTime, 'f', -1, 64)

	if response.Head != nil && response.Head.Params != nil {
		response.Head.Params["handlerCostTime"] = handlerCostTimeStr
	} else {
		costTimeMap := make(map[string]string)
		costTimeMap["handlerCostTime"] = handlerCostTimeStr
	}
	defer func() {
		if r := recover(); r != nil {
			response.Head.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: cast.ToString(r)}
		}
	}()
}

func bulkSearch(ctx context.Context, store PartitionStore, request []*vearchpb.SearchRequest, response []*vearchpb.SearchResponse) {
	wg := sync.WaitGroup{}
	for i, req := range request {
		wg.Add(1)
		go func(req *vearchpb.SearchRequest, resp *vearchpb.SearchResponse) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					if resp.Head == nil {
						responseHead := &vearchpb.ResponseHead{Err: &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: cast.ToString(r)}}
						resp.Head = responseHead
					}
				}
			}()

			if err := store.Search(ctx, req, resp); err != nil {
				log.Error("search doc failed, err: [%s]", err.Error())
				resp.Head.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
			}
		}(req, response[i])
	}
	wg.Wait()
}

func forceMerge(ctx context.Context, store PartitionStore, error *vearchpb.Error) {
	err := store.GetEngine().Optimize()
	if err != nil {
		partitionID := store.GetPartition().Id
		pIdStr := strconv.Itoa(int(partitionID))
		error = &vearchpb.Error{Code: vearchpb.ErrorEnum_FORCE_MERGE_BUILD_INDEX_ERR,
			Msg: "build index err, PartitionID :" + pIdStr}
	} else {
		error = nil
	}
}

func flush(ctx context.Context, store PartitionStore, error *vearchpb.Error) {
	err := store.Flush(ctx)
	if err != nil {
		partitionID := store.GetPartition().Id
		pIdStr := strconv.Itoa(int(partitionID))
		error = &vearchpb.Error{Code: vearchpb.ErrorEnum_FLUSH_ERR,
			Msg: "flush err, PartitionID :" + pIdStr}
	} else {
		error = nil
	}
}

func deleteByQuery(ctx context.Context, store PartitionStore, req *vearchpb.SearchRequest, resp *vearchpb.DelByQueryeResponse) {
	searchResponse := &vearchpb.SearchResponse{}
	if err := store.Search(ctx, req, searchResponse); err != nil {
		log.Error("deleteByQuery search doc failed, err: [%s]", err.Error())
		head := &vearchpb.ResponseHead{Err: &vearchpb.Error{Code: vearchpb.ErrorEnum_DELETE_BY_QUERY_SERACH_ERR, Msg: "deleteByQuery search doc failed"}}
		resp.Head = head
	} else {
		flatBytes := searchResponse.FlatBytes
		if flatBytes != nil {
			gamma.DeSerialize(flatBytes, searchResponse)
		}

		results := searchResponse.Results
		if results == nil || len(results) == 0 {
			head := &vearchpb.ResponseHead{Err: &vearchpb.Error{Code: vearchpb.ErrorEnum_DELETE_BY_QUERY_SEARCH_ID_IS_0, Msg: "deleteByQuery search id is 0"}}
			resp.Head = head
		} else {
			idIsLongStr := req.Head.Params["idIsLong"]

			idIsLong := false
			if idIsLongStr == "true" {
				idIsLong = true
			}
			docs := make([]*vearchpb.Item, 0)
			for _, result := range results {
				if result == nil || result.ResultItems == nil || len(result.ResultItems) == 0 {
					log.Error("query id is 0")
				} else {
					for _, doc := range result.ResultItems {
						var pKey string
						for _, fv := range doc.Fields {
							name := fv.Name
							switch name {
							case mapping.IdField:
								if idIsLong {
									id := int64(cbbytes.ByteArray2UInt64(fv.Value))
									pKey = strconv.FormatInt(id, 10)
								} else {
									pKey = string(fv.Value)
								}
							}
						}
						if pKey != "" {
							field := &vearchpb.Field{Name: "_id", Value: []byte(pKey)}
							fields := make([]*vearchpb.Field, 0)
							fields = append(fields, field)
							doc := &vearchpb.Document{PKey: pKey, Fields: fields}
							item := &vearchpb.Item{Doc: doc}
							docs = append(docs, item)
						}
					}
				}
			}
			if len(docs) == 0 {
				head := &vearchpb.ResponseHead{Err: &vearchpb.Error{Code: vearchpb.ErrorEnum_DELETE_BY_QUERY_SEARCH_ID_IS_0, Msg: "deleteByQuery search id is 0"}}
				resp.Head = head
			} else {
				deleteDocs(ctx, store, docs)
				for _, item := range docs {
					if item.Err == nil {
						resp.DelNum++
					}
				}
			}
		}
	}
}

func deleteByQueryFilter(ctx context.Context, store PartitionStore, request *vearchpb.SearchRequest, response *vearchpb.SearchResponse) {
	if err := store.Write(ctx, nil, request, response); err != nil {
		log.Error("deleteByQueryFilter doc failed, err: [%s]", err.Error())
		response.Head.Err = vearchpb.NewError(vearchpb.ErrorEnum_INTERNAL_ERROR, err).GetError()
	}
	defer func() {
		if r := recover(); r != nil {
			response.Head.Err = &vearchpb.Error{Code: vearchpb.ErrorEnum_INTERNAL_ERROR, Msg: cast.ToString(r)}
		}
	}()
}
