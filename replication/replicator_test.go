package replication

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/lindb/lindb/models"
	"github.com/lindb/lindb/pkg/queue"
	"github.com/lindb/lindb/rpc"
	"github.com/lindb/lindb/rpc/proto/storage"
)

/**
////
no replicas

case get remote nextSeq fail:
fct.CreateWriteServiceClient fail, wait 1 sec
fct.CreateWriteServiceClient success
nextSeq, err := r.remoteNextSeq fail
stop

case get remote nextSeq success, set local fanOut seq success:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient fail

case get remote nextSeq success, set local fanOut seq fail, set remote head seq fail:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) fail
r.resetRemoteSeq(r.fo.HeadSeq()) fail


case get remote nextSeq success, set local fanOut seq fail, set remote head seq success:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) fail
r.resetRemoteSeq(r.fo.HeadSeq()) fail
r.serviceClient.Reset(ctx, nextReq) success

////
with replicas

case normal replication, negotiation, set local fanOut seq success
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 5
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient fail
r.streamClient.Recv() block

fanOut consumer and get 5 ~ 20
stop

case replication seq not match, first set local fanOut seq to 5, second set to 7:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 5
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient success
r.streamClient.Recv() block, then return error
fanOut consumer and get 5 ~ 15

fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 17
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient success
r.streamClient.Recv() block, then return error
fanOut consumer and get 7 ~ 15

stop


*/

var (
	node = models.Node{
		IP:   "123",
		Port: 123,
	}
	database = "database"
	shardID  = int32(0)
)

func buildWriteRequest(seqBegin, seqEnd int64) (*storage.WriteRequest, string) {
	replicas := make([]*storage.Replica, seqEnd-seqBegin)
	for i := seqBegin; i < seqEnd; i++ {
		replicas[i-seqBegin] = &storage.Replica{
			Seq:  i,
			Data: []byte(strconv.Itoa(int(i))),
		}
	}
	wr := &storage.WriteRequest{
		Replicas: replicas,
	}
	return wr, fmt.Sprintf("[%d,%d)", seqBegin, seqEnd)
}

func TestSimple(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(nil, errors.New("get service client error")).AnyTimes()

	rep := newReplicator(node, database, shardID, nil, mockFct)

	assert.Equal(t, database, rep.Database())
	assert.Equal(t, shardID, rep.ShardID())
	assert.Equal(t, node, rep.Target())

	rep.Stop()
}

/**
case get remote nextSeq fail:
fct.CreateWriteServiceClient fail, wait 1 sec
fct.CreateWriteServiceClient success
nextSeq, err := r.remoteNextSeq fail
stop
*/
func TestGetRemoteNextSeqFail(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(nil, errors.New("get remote next seq error"))

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(nil, errors.New("get service client error"))
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node)

	done := make(chan struct{})
	mockFct.EXPECT().CreateWriteServiceClient(node).DoAndReturn(func(node models.Node) (storage.WriteServiceClient, error) {
		close(done)
		// wait for <- done to stop replica
		time.Sleep(100 * time.Millisecond)
		return nil, errors.New("get service client error any")
	})

	rep := newReplicator(node, database, shardID, nil, mockFct)
	// if the main go-routine is block, check mock call missing work will be block too.
	<-done
	rep.Stop()
}

/**
case get remote nextSeq success, set local fanOut seq fail:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) fail
r.resetRemoteSeq(r.fo.HeadSeq()) fail
*/
func TestSetLocalHeadSeqFail(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: 0,
	}, nil)
	mockServiceClient.EXPECT().Reset(gomock.Any(), gomock.Any()).Return(nil, errors.New("reset remote next seq error"))

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node).Times(2)

	done := make(chan struct{})
	mockFct.EXPECT().CreateWriteServiceClient(node).DoAndReturn(func(_ models.Node) (storage.WriteServiceClient, error) {
		close(done)
		// wait for <- done to stop replica
		time.Sleep(100 * time.Millisecond)
		return nil, errors.New("get service client error any")
	})

	mockFanOut := queue.NewMockFanOut(ctl)
	mockFanOut.EXPECT().SetHeadSeq(gomock.Any()).Return(errors.New("fanOut set head seq error"))
	mockFanOut.EXPECT().HeadSeq().Return(int64(0))

	rep := newReplicator(node, database, shardID, mockFanOut, mockFct)

	<-done
	rep.Stop()
}

/**
case get remote nextSeq success, set local fanOut seq success:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) success
*/
func TestSetLocalHeadSeqSuccess(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	nextSeq := int64(5)
	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: nextSeq,
	}, nil)

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node)
	mockFct.EXPECT().CreateWriteClient(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("create stream client error"))

	done := make(chan struct{})
	mockFct.EXPECT().CreateWriteServiceClient(node).DoAndReturn(func(_ models.Node) (storage.WriteServiceClient, error) {
		close(done)
		// wait for <- done to stop replica
		time.Sleep(100 * time.Millisecond)
		return nil, errors.New("get service client error any")
	})

	mockFanOut := queue.NewMockFanOut(ctl)
	mockFanOut.EXPECT().SetHeadSeq(nextSeq).Return(nil)

	rep := newReplicator(node, database, shardID, mockFanOut, mockFct)

	<-done
	rep.Stop()
}

/**
case get remote nextSeq success, set local fanOut seq fail, set remote head seq success:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success
r.fo.SetHeadSeq(nextSeq) fail
r.resetRemoteSeq(r.fo.HeadSeq()) fail
r.serviceClient.Reset(ctx, nextReq) success
*/
func TestResetRemoteSeqSuccess(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: 0,
	}, nil)
	mockServiceClient.EXPECT().Reset(gomock.Any(), gomock.Any()).Return(&storage.ResetSeqResponse{}, nil)

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node).Times(2)
	mockFct.EXPECT().CreateWriteClient(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("creat write client error"))

	done := make(chan struct{})
	mockFct.EXPECT().CreateWriteServiceClient(node).DoAndReturn(func(_ models.Node) (storage.WriteServiceClient, error) {
		close(done)
		time.Sleep(100 * time.Millisecond)
		return nil, errors.New("get service client error any")
	})

	mockFanOut := queue.NewMockFanOut(ctl)
	mockFanOut.EXPECT().SetHeadSeq(gomock.Any()).Return(errors.New("fanOut set head seq error"))
	mockFanOut.EXPECT().HeadSeq().Return(int64(0))

	rep := newReplicator(node, database, shardID, mockFanOut, mockFct)

	<-done
	rep.Stop()
}

/**
case normal replication, negotiation, set local fanOut seq success
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 5
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient fail
r.streamClient.Recv() block

fanOut consumer and get 5 ~ 20
stop
*/
func TestNormalReplication(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	nextSeq := int64(5)
	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: nextSeq,
	}, nil)

	done := make(chan struct{})
	mockClientStream := storage.NewMockWriteService_WriteClient(ctl)
	mockClientStream.EXPECT().Recv().DoAndReturn(func() (*storage.WriteResponse, error) {
		<-done
		return nil, errors.New("stream canceled")
	})

	// replica 5~15
	wr1, _ := buildWriteRequest(5, 15)
	mockClientStream.EXPECT().Send(wr1).Return(nil)

	// replica 15 ~ 20
	wr2, _ := buildWriteRequest(15, 20)
	mockClientStream.EXPECT().Send(wr2).Return(nil)

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node)
	mockFct.EXPECT().CreateWriteClient(database, shardID, node).Return(mockClientStream, nil)

	mockFanOut := queue.NewMockFanOut(ctl)
	mockFanOut.EXPECT().SetHeadSeq(nextSeq).Return(nil)

	for i := 5; i < 20; i++ {
		mockFanOut.EXPECT().Consume().Return(int64(i))
		mockFanOut.EXPECT().Get(int64(i)).Return([]byte(strconv.Itoa(i)), nil)
	}
	mockFanOut.EXPECT().Consume().Return(queue.SeqNoNewMessageAvailable).AnyTimes()

	rep := newReplicator(node, database, shardID, mockFanOut, mockFct)

	time.Sleep(time.Second * 2)
	rep.Stop()
	close(done)
}

/**
case replication seq not match, first set local fanOut seq to 5, second set to 7:
fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 5
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient success
r.streamClient.Recv() block, then return error
fanOut consumer and get 5 ~ 15

fct.CreateWriteServiceClient success
r.serviceClient.Next(ctx, nextReq) success next = 17
r.fo.SetHeadSeq(nextSeq) success
r.fct.CreateWriteClient success
r.streamClient.Recv() block, then return error
fanOut consumer and get 7 ~ 15

stop
*/
func TestReplicationSeqNotMatch(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	mockServiceClient := storage.NewMockWriteServiceClient(ctl)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: 5,
	}, nil)
	mockServiceClient.EXPECT().Next(gomock.Any(), gomock.Any()).Return(&storage.NextSeqResponse{
		Seq: 7,
	}, nil)

	done1 := make(chan struct{})
	done2 := make(chan struct{})
	mockClientStream := storage.NewMockWriteService_WriteClient(ctl)
	mockClientStream.EXPECT().Recv().DoAndReturn(func() (*storage.WriteResponse, error) {
		<-done1
		time.Sleep(10 * time.Millisecond)
		return nil, errors.New("stream canceled")
	})

	// replica 5~15
	wr1, _ := buildWriteRequest(5, 15)
	mockClientStream.EXPECT().Send(wr1).DoAndReturn(func(_ *storage.WriteRequest) error {
		// notify recv loop to re-connect
		close(done1)
		return errors.New("seq not match")
	})

	mockClientStream.EXPECT().Recv().DoAndReturn(func() (*storage.WriteResponse, error) {
		<-done2
		return nil, errors.New("stream canceled")
	})

	// replica 7 ~ 15
	wr2, _ := buildWriteRequest(7, 15)
	mockClientStream.EXPECT().Send(wr2).Return(nil)

	mockFct := rpc.NewMockClientStreamFactory(ctl)
	// first time
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node)
	mockFct.EXPECT().CreateWriteClient(database, shardID, node).Return(mockClientStream, nil)
	// second time
	mockFct.EXPECT().CreateWriteServiceClient(node).Return(mockServiceClient, nil)
	mockFct.EXPECT().LogicNode().Return(node)
	mockFct.EXPECT().CreateWriteClient(database, shardID, node).Return(mockClientStream, nil)

	mockFanOut := queue.NewMockFanOut(ctl)
	mockFanOut.EXPECT().SetHeadSeq(int64(5)).Return(nil)
	mockFanOut.EXPECT().SetHeadSeq(int64(7)).Return(nil)
	// first time
	for i := 5; i < 15; i++ {
		mockFanOut.EXPECT().Consume().Return(int64(i))
		mockFanOut.EXPECT().Get(int64(i)).Return([]byte(strconv.Itoa(i)), nil)
	}

	// second time
	for i := 7; i < 15; i++ {
		mockFanOut.EXPECT().Consume().Return(int64(i))
		mockFanOut.EXPECT().Get(int64(i)).Return([]byte(strconv.Itoa(i)), nil)
	}
	mockFanOut.EXPECT().Consume().Return(queue.SeqNoNewMessageAvailable).AnyTimes()

	rep := newReplicator(node, database, shardID, mockFanOut, mockFct)

	time.Sleep(time.Second * 4)
	rep.Stop()
	close(done2)
}
