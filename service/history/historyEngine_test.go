// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	commonpb "go.temporal.io/temporal-proto/common"
	decisionpb "go.temporal.io/temporal-proto/decision"
	eventpb "go.temporal.io/temporal-proto/event"
	executionpb "go.temporal.io/temporal-proto/execution"
	namespacepb "go.temporal.io/temporal-proto/namespace"
	querypb "go.temporal.io/temporal-proto/query"
	"go.temporal.io/temporal-proto/serviceerror"
	tasklistpb "go.temporal.io/temporal-proto/tasklist"
	"go.temporal.io/temporal-proto/workflowservice"

	executiongenpb "github.com/temporalio/temporal/.gen/proto/execution"
	"github.com/temporalio/temporal/.gen/proto/historyservice"
	"github.com/temporalio/temporal/.gen/proto/historyservicemock"
	"github.com/temporalio/temporal/.gen/proto/matchingservice"
	"github.com/temporalio/temporal/.gen/proto/matchingservicemock"
	"github.com/temporalio/temporal/.gen/proto/persistenceblobs"
	replicationgenpb "github.com/temporalio/temporal/.gen/proto/replication"
	tokengenpb "github.com/temporalio/temporal/.gen/proto/token"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/clock"
	"github.com/temporalio/temporal/common/cluster"
	"github.com/temporalio/temporal/common/headers"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/loggerimpl"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/mocks"
	"github.com/temporalio/temporal/common/payload"
	"github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/primitives"
	cconfig "github.com/temporalio/temporal/common/service/config"
	"github.com/temporalio/temporal/common/service/dynamicconfig"
)

type (
	engineSuite struct {
		suite.Suite
		*require.Assertions

		controller               *gomock.Controller
		mockShard                *shardContextTest
		mockTxProcessor          *MocktransferQueueProcessor
		mockReplicationProcessor *MockReplicatorQueueProcessor
		mockTimerProcessor       *MocktimerQueueProcessor
		mockNamespaceCache       *cache.MockNamespaceCache
		mockMatchingClient       *matchingservicemock.MockMatchingServiceClient
		mockHistoryClient        *historyservicemock.MockHistoryServiceClient
		mockClusterMetadata      *cluster.MockMetadata

		mockHistoryEngine *historyEngineImpl
		mockExecutionMgr  *mocks.ExecutionManager
		mockHistoryV2Mgr  *mocks.HistoryV2Manager
		mockShardManager  *mocks.ShardManager

		eventsCache eventsCache
		config      *Config
	}
)

var testVersion = int64(1234)
var testNamespaceID = "deadbeef-0123-4567-890a-bcdef0123456"
var testNamespaceUUID = primitives.MustParseUUID(testNamespaceID)
var testNamespace = "some random namespace name"
var testParentNamespaceID = "deadbeef-0123-4567-890a-bcdef0123457"
var testParentNamespaceUUID = primitives.MustParseUUID(testParentNamespaceID)
var testParentNamespace = "some random parent namespace name"
var testTargetNamespaceID = "deadbeef-0123-4567-890a-bcdef0123458"
var testTargetNamespaceUUID = primitives.MustParseUUID(testTargetNamespaceID)
var testTargetNamespace = "some random target namespace name"
var testChildNamespaceID = "deadbeef-0123-4567-890a-bcdef0123459"
var testChildNamespaceUUID = primitives.MustParseUUID(testChildNamespaceID)
var testChildNamespace = "some random child namespace name"
var testWorkflowID = "random-workflow-id"
var testRunID = "0d00698f-08e1-4d36-a3e2-3bf109f5d2d6"

var testLocalNamespaceEntry = cache.NewLocalNamespaceCacheEntryForTest(
	&persistenceblobs.NamespaceInfo{Id: testNamespaceUUID, Name: testNamespace},
	&persistenceblobs.NamespaceConfig{RetentionDays: 1},
	cluster.TestCurrentClusterName,
	nil,
)

var testGlobalNamespaceEntry = cache.NewGlobalNamespaceCacheEntryForTest(
	&persistenceblobs.NamespaceInfo{Id: testNamespaceUUID, Name: testNamespace},
	&persistenceblobs.NamespaceConfig{
		RetentionDays:            1,
		VisibilityArchivalStatus: namespacepb.ArchivalStatus_Enabled,
		VisibilityArchivalURI:    "test:///visibility/archival",
	},
	&persistenceblobs.NamespaceReplicationConfig{
		ActiveClusterName: cluster.TestCurrentClusterName,
		Clusters: []string{
			cluster.TestCurrentClusterName,
			cluster.TestAlternativeClusterName,
		},
	},
	testVersion,
	nil,
)

var testGlobalParentNamespaceEntry = cache.NewGlobalNamespaceCacheEntryForTest(
	&persistenceblobs.NamespaceInfo{Id: testParentNamespaceUUID, Name: testParentNamespace},
	&persistenceblobs.NamespaceConfig{RetentionDays: 1},
	&persistenceblobs.NamespaceReplicationConfig{
		ActiveClusterName: cluster.TestCurrentClusterName,
		Clusters: []string{
			cluster.TestCurrentClusterName,
			cluster.TestAlternativeClusterName,
		},
	},
	testVersion,
	nil,
)

var testGlobalTargetNamespaceEntry = cache.NewGlobalNamespaceCacheEntryForTest(
	&persistenceblobs.NamespaceInfo{Id: testTargetNamespaceUUID, Name: testTargetNamespace},
	&persistenceblobs.NamespaceConfig{RetentionDays: 1},
	&persistenceblobs.NamespaceReplicationConfig{
		ActiveClusterName: cluster.TestCurrentClusterName,
		Clusters: []string{
			cluster.TestCurrentClusterName,
			cluster.TestAlternativeClusterName,
		},
	},
	testVersion,
	nil,
)

var testGlobalChildNamespaceEntry = cache.NewGlobalNamespaceCacheEntryForTest(
	&persistenceblobs.NamespaceInfo{Id: testChildNamespaceUUID, Name: testChildNamespace},
	&persistenceblobs.NamespaceConfig{RetentionDays: 1},
	&persistenceblobs.NamespaceReplicationConfig{
		ActiveClusterName: cluster.TestCurrentClusterName,
		Clusters: []string{
			cluster.TestCurrentClusterName,
			cluster.TestAlternativeClusterName,
		},
	},
	testVersion,
	nil,
)

func NewDynamicConfigForTest() *Config {
	dc := dynamicconfig.NewNopCollection()
	config := NewConfig(dc, 1, cconfig.StoreTypeCassandra, false)
	// reduce the duration of long poll to increase test speed
	config.LongPollExpirationInterval = dc.GetDurationPropertyFilteredByNamespace(dynamicconfig.HistoryLongPollExpirationInterval, 10*time.Second)
	config.EnableConsistentQueryByNamespace = dc.GetBoolPropertyFnWithNamespaceFilter(dynamicconfig.EnableConsistentQueryByNamespace, true)
	return config
}

func TestEngineSuite(t *testing.T) {
	s := new(engineSuite)
	suite.Run(t, s)
}

func (s *engineSuite) SetupSuite() {
	s.config = NewDynamicConfigForTest()
}

func (s *engineSuite) TearDownSuite() {
}

func (s *engineSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockTxProcessor = NewMocktransferQueueProcessor(s.controller)
	s.mockReplicationProcessor = NewMockReplicatorQueueProcessor(s.controller)
	s.mockTimerProcessor = NewMocktimerQueueProcessor(s.controller)
	s.mockTxProcessor.EXPECT().NotifyNewTask(gomock.Any(), gomock.Any()).AnyTimes()
	s.mockReplicationProcessor.EXPECT().notifyNewTask().AnyTimes()
	s.mockTimerProcessor.EXPECT().NotifyNewTimers(gomock.Any(), gomock.Any()).AnyTimes()

	s.mockShard = newTestShardContext(
		s.controller,
		&persistence.ShardInfoWithFailover{
			ShardInfo: &persistenceblobs.ShardInfo{
				RangeId:          1,
				TransferAckLevel: 0,
			}},
		s.config,
	)
	s.eventsCache = newEventsCache(s.mockShard)
	s.mockShard.eventsCache = s.eventsCache

	s.mockMatchingClient = s.mockShard.resource.MatchingClient
	s.mockHistoryClient = s.mockShard.resource.HistoryClient
	s.mockExecutionMgr = s.mockShard.resource.ExecutionMgr
	s.mockHistoryV2Mgr = s.mockShard.resource.HistoryMgr
	s.mockShardManager = s.mockShard.resource.ShardMgr
	s.mockClusterMetadata = s.mockShard.resource.ClusterMetadata
	s.mockNamespaceCache = s.mockShard.resource.NamespaceCache
	s.mockClusterMetadata.EXPECT().IsGlobalNamespaceEnabled().Return(false).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetCurrentClusterName().Return(cluster.TestCurrentClusterName).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetAllClusterInfo().Return(cluster.TestSingleDCClusterInfo).AnyTimes()
	s.mockClusterMetadata.EXPECT().ClusterNameForFailoverVersion(common.EmptyVersion).Return(cluster.TestCurrentClusterName).AnyTimes()
	s.mockNamespaceCache.EXPECT().GetNamespaceByID(testNamespaceID).Return(testLocalNamespaceEntry, nil).AnyTimes()
	s.mockNamespaceCache.EXPECT().GetNamespace(testNamespace).Return(testLocalNamespaceEntry, nil).AnyTimes()

	historyEventNotifier := newHistoryEventNotifier(
		clock.NewRealTimeSource(),
		s.mockShard.resource.MetricsClient,
		func(workflowID string) int {
			return len(workflowID)
		},
	)

	historyCache := newHistoryCache(s.mockShard)
	h := &historyEngineImpl{
		currentClusterName:   s.mockShard.GetClusterMetadata().GetCurrentClusterName(),
		shard:                s.mockShard,
		clusterMetadata:      s.mockClusterMetadata,
		executionManager:     s.mockExecutionMgr,
		historyV2Mgr:         s.mockHistoryV2Mgr,
		historyCache:         historyCache,
		logger:               s.mockShard.GetLogger(),
		metricsClient:        s.mockShard.GetMetricsClient(),
		tokenSerializer:      common.NewProtoTaskTokenSerializer(),
		historyEventNotifier: historyEventNotifier,
		config:               NewDynamicConfigForTest(),
		txProcessor:          s.mockTxProcessor,
		replicatorProcessor:  s.mockReplicationProcessor,
		timerProcessor:       s.mockTimerProcessor,
		versionChecker:       headers.NewVersionChecker(),
	}
	s.mockShard.SetEngine(h)
	h.decisionHandler = newDecisionHandler(h)

	h.historyEventNotifier.Start()

	s.mockHistoryEngine = h
}

func (s *engineSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.Finish(s.T())
	s.mockHistoryEngine.historyEventNotifier.Stop()
}

func (s *engineSuite) TestGetMutableStateSync() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	// right now the next event ID is 4
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	// test get the next event ID instantly
	response, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId: testNamespaceID,
		Execution:   &execution,
	})
	s.Nil(err)
	s.Equal(int64(4), response.GetNextEventId())
}

func (s *engineSuite) TestGetMutableState_IntestRunID() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
		RunId:      "run-id-not-valid-uuid",
	}

	_, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId: testNamespaceID,
		Execution:   &execution,
	})
	s.Equal(errRunIDNotValid, err)
}

func (s *engineSuite) TestGetMutableState_EmptyRunID() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
	}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(nil, serviceerror.NewNotFound("")).Once()

	_, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId: testNamespaceID,
		Execution:   &execution,
	})
	s.Equal(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestGetMutableStateLongPoll() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	// right now the next event ID is 4
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	// test long poll on next event ID change
	waitGroup := &sync.WaitGroup{}
	waitGroup.Add(1)
	asycWorkflowUpdate := func(delay time.Duration) {
		tt := &tokengenpb.Task{
			WorkflowId: execution.WorkflowId,
			RunId:      primitives.MustParseUUID(execution.RunId),
			ScheduleId: 2,
		}
		taskToken, _ := tt.Marshal()
		s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
		s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

		timer := time.NewTimer(delay)

		<-timer.C
		_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
			NamespaceId: testNamespaceID,
			CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
				TaskToken: taskToken,
				Identity:  identity,
			},
		})
		s.Nil(err)
		waitGroup.Done()
		// right now the next event ID is 5
	}

	// return immediately, since the expected next event ID appears
	response, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId:         testNamespaceID,
		Execution:           &execution,
		ExpectedNextEventId: 3,
	})
	s.Nil(err)
	s.Equal(int64(4), response.NextEventId)

	// long poll, new event happen before long poll timeout
	go asycWorkflowUpdate(time.Second * 2)
	start := time.Now()
	pollResponse, err := s.mockHistoryEngine.PollMutableState(ctx, &historyservice.PollMutableStateRequest{
		NamespaceId:         testNamespaceID,
		Execution:           &execution,
		ExpectedNextEventId: 4,
	})
	s.True(time.Now().After(start.Add(time.Second * 1)))
	s.Nil(err)
	s.Equal(int64(5), pollResponse.GetNextEventId())
	waitGroup.Wait()
}

func (s *engineSuite) TestGetMutableStateLongPoll_CurrentBranchChanged() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(
		s.mockHistoryEngine.shard,
		s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite),
		execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	// right now the next event ID is 4
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	// test long poll on next event ID change
	asyncBranchTokenUpdate := func(delay time.Duration) {
		timer := time.NewTimer(delay)
		<-timer.C
		newExecution := &executionpb.WorkflowExecution{
			WorkflowId: execution.WorkflowId,
			RunId:      execution.RunId,
		}
		s.mockHistoryEngine.historyEventNotifier.NotifyNewHistoryEvent(newHistoryEventNotification(
			"testNamespaceID",
			newExecution,
			int64(1),
			int64(4),
			int64(1),
			[]byte{1},
			persistence.WorkflowStateCreated,
			executionpb.WorkflowExecutionStatus_Running))
	}

	// return immediately, since the expected next event ID appears
	response0, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId:         testNamespaceID,
		Execution:           &execution,
		ExpectedNextEventId: 3,
	})
	s.Nil(err)
	s.Equal(int64(4), response0.GetNextEventId())

	// long poll, new event happen before long poll timeout
	go asyncBranchTokenUpdate(time.Second * 2)
	start := time.Now()
	response1, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId:         testNamespaceID,
		Execution:           &execution,
		ExpectedNextEventId: 10,
	})
	s.True(time.Now().After(start.Add(time.Second * 1)))
	s.Nil(err)
	s.Equal(response0.GetCurrentBranchToken(), response1.GetCurrentBranchToken())
}

func (s *engineSuite) TestGetMutableStateLongPollTimeout() {
	ctx := context.Background()

	execution := executionpb.WorkflowExecution{
		WorkflowId: "test-get-workflow-execution-event-id",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	// right now the next event ID is 4
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	// long poll, no event happen after long poll timeout
	response, err := s.mockHistoryEngine.GetMutableState(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId:         testNamespaceID,
		Execution:           &execution,
		ExpectedNextEventId: 4,
	})
	s.Nil(err)
	s.Equal(int64(4), response.GetNextEventId())
}

func (s *engineSuite) TestQueryWorkflow_RejectBasedOnNotEnabled() {
	s.mockHistoryEngine.config.EnableConsistentQueryByNamespace = dynamicconfig.GetBoolPropertyFnFilteredByNamespace(false)
	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Strong,
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.Nil(resp)
	s.Equal(ErrConsistentQueryNotEnabled, err)

	s.mockHistoryEngine.config.EnableConsistentQueryByNamespace = dynamicconfig.GetBoolPropertyFnFilteredByNamespace(true)
	s.mockHistoryEngine.config.EnableConsistentQuery = dynamicconfig.GetBoolPropertyFn(false)
	resp, err = s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.Nil(resp)
	s.Equal(ErrConsistentQueryNotEnabled, err)
}

func (s *engineSuite) TestQueryWorkflow_RejectBasedOnCompleted() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_RejectBasedOnCompleted",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, "some random identity")
	addCompleteWorkflowEvent(msBuilder, event.GetEventId(), nil)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:            &execution,
			Query:                &querypb.WorkflowQuery{},
			QueryRejectCondition: querypb.QueryRejectCondition_NotOpen,
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.NoError(err)
	s.Nil(resp.GetResponse().QueryResult)
	s.NotNil(resp.GetResponse().QueryRejected)
	s.Equal(executionpb.WorkflowExecutionStatus_Completed, resp.GetResponse().GetQueryRejected().GetStatus())
}

func (s *engineSuite) TestQueryWorkflow_RejectBasedOnFailed() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_RejectBasedOnFailed",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	event := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, di.StartedID, "some random identity")
	addFailWorkflowEvent(msBuilder, event.GetEventId(), "failure reason", payload.EncodeBytes([]byte{1, 2, 3}))
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:            &execution,
			Query:                &querypb.WorkflowQuery{},
			QueryRejectCondition: querypb.QueryRejectCondition_NotOpen,
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.NoError(err)
	s.Nil(resp.GetResponse().QueryResult)
	s.NotNil(resp.GetResponse().QueryRejected)
	s.Equal(executionpb.WorkflowExecutionStatus_Failed, resp.GetResponse().GetQueryRejected().GetStatus())

	request = &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:            &execution,
			Query:                &querypb.WorkflowQuery{},
			QueryRejectCondition: querypb.QueryRejectCondition_NotCompletedCleanly,
		},
	}
	resp, err = s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.NoError(err)
	s.Nil(resp.GetResponse().QueryResult)
	s.NotNil(resp.GetResponse().QueryRejected)
	s.Equal(executionpb.WorkflowExecutionStatus_Failed, resp.GetResponse().GetQueryRejected().GetStatus())
}

func (s *engineSuite) TestQueryWorkflow_FirstDecisionNotCompleted() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_FirstDecisionNotCompleted",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution: &execution,
			Query:     &querypb.WorkflowQuery{},
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.Equal(ErrQueryWorkflowBeforeFirstDecision, err)
	s.Nil(resp)
}

func (s *engineSuite) TestQueryWorkflow_DirectlyThroughMatching() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_DirectlyThroughMatching",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)
	di = addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)

	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()
	s.mockMatchingClient.EXPECT().QueryWorkflow(gomock.Any(), gomock.Any()).Return(&matchingservice.QueryWorkflowResponse{QueryResult: payload.EncodeBytes([]byte{1, 2, 3})}, nil)
	s.mockHistoryEngine.matchingClient = s.mockMatchingClient
	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution: &execution,
			Query:     &querypb.WorkflowQuery{},
			// since workflow is open this filter does not reject query
			QueryRejectCondition:  querypb.QueryRejectCondition_NotOpen,
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Eventual,
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.NoError(err)
	s.NotNil(resp.GetResponse().QueryResult)
	s.Nil(resp.GetResponse().QueryRejected)

	var queryResult []byte
	err = payload.Decode(resp.GetResponse().GetQueryResult(), &queryResult)
	s.NoError(err)
	s.Equal([]byte{1, 2, 3}, queryResult)
}

func (s *engineSuite) TestQueryWorkflow_DecisionTaskDispatch_Timeout() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_DecisionTaskDispatch_Timeout",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)
	di = addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)

	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()
	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution: &execution,
			Query:     &querypb.WorkflowQuery{},
			// since workflow is open this filter does not reject query
			QueryRejectCondition:  querypb.QueryRejectCondition_NotOpen,
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Strong,
		},
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
		defer cancel()
		resp, err := s.mockHistoryEngine.QueryWorkflow(ctx, request)
		s.Error(err)
		s.Nil(resp)
		wg.Done()
	}()

	<-time.After(time.Second)
	builder := s.getBuilder(testNamespaceID, execution)
	s.NotNil(builder)
	qr := builder.GetQueryRegistry()
	s.True(qr.hasBufferedQuery())
	s.False(qr.hasCompletedQuery())
	s.False(qr.hasUnblockedQuery())
	s.False(qr.hasFailedQuery())
	wg.Wait()
	s.False(qr.hasBufferedQuery())
	s.False(qr.hasCompletedQuery())
	s.False(qr.hasUnblockedQuery())
	s.False(qr.hasFailedQuery())
}

func (s *engineSuite) TestQueryWorkflow_ConsistentQueryBufferFull() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_ConsistentQueryBufferFull",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)
	di = addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)

	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	// buffer query so that when history.QueryWorkflow is called buffer is already full
	ctx, release, err := s.mockHistoryEngine.historyCache.getOrCreateWorkflowExecutionForBackground(testNamespaceID, execution)
	s.NoError(err)
	loadedMS, err := ctx.loadWorkflowExecution()
	s.NoError(err)
	qr := newQueryRegistry()
	qr.bufferQuery(&querypb.WorkflowQuery{})
	loadedMS.(*mutableStateBuilder).queryRegistry = qr
	release(nil)

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:             &execution,
			Query:                 &querypb.WorkflowQuery{},
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Strong,
		},
	}
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.Nil(resp)
	s.Equal(ErrConsistentQueryBufferExceeded, err)
}

func (s *engineSuite) TestQueryWorkflow_DecisionTaskDispatch_Complete() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_DecisionTaskDispatch_Complete",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)
	di = addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)

	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()

	waitGroup := &sync.WaitGroup{}
	waitGroup.Add(1)
	asyncQueryUpdate := func(delay time.Duration, answer []byte) {
		defer waitGroup.Done()
		<-time.After(delay)
		builder := s.getBuilder(testNamespaceID, execution)
		s.NotNil(builder)
		qr := builder.GetQueryRegistry()
		buffered := qr.getBufferedIDs()
		for _, id := range buffered {
			resultType := querypb.QueryResultType_Answered
			completedTerminationState := &queryTerminationState{
				queryTerminationType: queryTerminationTypeCompleted,
				queryResult: &querypb.WorkflowQueryResult{
					ResultType: resultType,
					Answer:     payload.EncodeBytes(answer),
				},
			}
			err := qr.setTerminationState(id, completedTerminationState)
			s.NoError(err)
			state, err := qr.getTerminationState(id)
			s.NoError(err)
			s.Equal(queryTerminationTypeCompleted, state.queryTerminationType)
		}
	}

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:             &execution,
			Query:                 &querypb.WorkflowQuery{},
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Strong,
		},
	}
	go asyncQueryUpdate(time.Second*2, []byte{1, 2, 3})
	start := time.Now()
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.True(time.Now().After(start.Add(time.Second)))
	s.NoError(err)

	var queryResult []byte
	err = payload.Decode(resp.GetResponse().GetQueryResult(), &queryResult)
	s.NoError(err)
	s.Equal([]byte{1, 2, 3}, queryResult)

	builder := s.getBuilder(testNamespaceID, execution)
	s.NotNil(builder)
	qr := builder.GetQueryRegistry()
	s.False(qr.hasBufferedQuery())
	s.False(qr.hasCompletedQuery())
	waitGroup.Wait()
}

func (s *engineSuite) TestQueryWorkflow_DecisionTaskDispatch_Unblocked() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "TestQueryWorkflow_DecisionTaskDispatch_Unblocked",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache, loggerimpl.NewDevelopmentForTest(s.Suite), execution.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)
	di = addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tasklist, identity)

	ms := createMutableState(msBuilder)
	gweResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gweResponse, nil).Once()
	s.mockMatchingClient.EXPECT().QueryWorkflow(gomock.Any(), gomock.Any()).Return(&matchingservice.QueryWorkflowResponse{QueryResult: payload.EncodeBytes([]byte{1, 2, 3})}, nil)
	s.mockHistoryEngine.matchingClient = s.mockMatchingClient
	waitGroup := &sync.WaitGroup{}
	waitGroup.Add(1)
	asyncQueryUpdate := func(delay time.Duration, answer []byte) {
		defer waitGroup.Done()
		<-time.After(delay)
		builder := s.getBuilder(testNamespaceID, execution)
		s.NotNil(builder)
		qr := builder.GetQueryRegistry()
		buffered := qr.getBufferedIDs()
		for _, id := range buffered {
			s.NoError(qr.setTerminationState(id, &queryTerminationState{queryTerminationType: queryTerminationTypeUnblocked}))
			state, err := qr.getTerminationState(id)
			s.NoError(err)
			s.Equal(queryTerminationTypeUnblocked, state.queryTerminationType)
		}
	}

	request := &historyservice.QueryWorkflowRequest{
		NamespaceId: testNamespaceID,
		Request: &workflowservice.QueryWorkflowRequest{
			Execution:             &execution,
			Query:                 &querypb.WorkflowQuery{},
			QueryConsistencyLevel: querypb.QueryConsistencyLevel_Strong,
		},
	}
	go asyncQueryUpdate(time.Second*2, []byte{1, 2, 3})
	start := time.Now()
	resp, err := s.mockHistoryEngine.QueryWorkflow(context.Background(), request)
	s.True(time.Now().After(start.Add(time.Second)))
	s.NoError(err)

	var queryResult []byte
	err = payload.Decode(resp.GetResponse().GetQueryResult(), &queryResult)
	s.NoError(err)
	s.Equal([]byte{1, 2, 3}, queryResult)

	builder := s.getBuilder(testNamespaceID, execution)
	s.NotNil(builder)
	qr := builder.GetQueryRegistry()
	s.False(qr.hasBufferedQuery())
	s.False(qr.hasCompletedQuery())
	s.False(qr.hasUnblockedQuery())
	waitGroup.Wait()
}

func (s *engineSuite) TestRespondDecisionTaskCompletedInvalidToken() {

	invalidToken, _ := json.Marshal("bad token")
	identity := "testIdentity"

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: invalidToken,
			Decisions: nil,
			Identity:  identity,
		},
	})

	s.NotNil(err)
	s.IsType(&serviceerror.InvalidArgument{}, err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedIfNoExecution() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, serviceerror.NewNotFound("")).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedIfGetExecutionFailed() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, errors.New("FAILED")).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondDecisionTaskCompletedUpdateExecutionFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"

	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, errors.New("FAILED")).Once()
	s.mockShardManager.On("UpdateShard", mock.Anything).Return(nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondDecisionTaskCompletedIfTaskCompleted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	startedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, startedEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedIfTaskNotStarted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedConflictOnUpdate() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	activity1Result := payload.EncodeString("activity1_result")
	activity2ID := "activity2"
	activity2Type := "activity_type2"
	activity2Input := payload.EncodeString("input2")
	activity2Result := payload.EncodeString("activity2_result")
	activity3ID := "activity3"
	activity3Type := "activity_type3"
	activity3Input := payload.EncodeString("input3")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di1 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di1.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di1.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId,
		activity1ID, activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity2ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId,
		activity2ID, activity2Type, tl, activity2Input, 100, 10, 1, 5)
	activity1StartedEvent := addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	activity2StartedEvent := addActivityTaskStartedEvent(msBuilder, activity2ScheduledEvent.EventId, identity)
	addActivityTaskCompletedEvent(msBuilder, activity1ScheduledEvent.EventId,
		activity1StartedEvent.EventId, activity1Result, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent2 := addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: di2.ScheduleID,
	}
	taskToken, _ := tt.Marshal()

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_ScheduleActivityTask,
		Attributes: &decisionpb.Decision_ScheduleActivityTaskDecisionAttributes{ScheduleActivityTaskDecisionAttributes: &decisionpb.ScheduleActivityTaskDecisionAttributes{
			ActivityId:                    activity3ID,
			ActivityType:                  &commonpb.ActivityType{Name: activity3Type},
			TaskList:                      &tasklistpb.TaskList{Name: tl},
			Input:                         activity3Input,
			ScheduleToCloseTimeoutSeconds: 100,
			ScheduleToStartTimeoutSeconds: 10,
			StartToCloseTimeoutSeconds:    50,
			HeartbeatTimeoutSeconds:       5,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	addActivityTaskCompletedEvent(msBuilder, activity2ScheduledEvent.EventId,
		activity2StartedEvent.EventId, activity2Result, identity)

	ms2 := createMutableState(msBuilder)
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: ms2}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}},
		&persistence.ConditionFailedError{}).Once()

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	s.Equal(int64(16), ms2.ExecutionInfo.NextEventID)
	s.Equal(decisionStartedEvent2.EventId, ms2.ExecutionInfo.LastProcessedEvent)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	activity3Attributes := s.getActivityScheduledEvent(executionBuilder, 13).GetActivityTaskScheduledEventAttributes()
	s.Equal(activity3ID, activity3Attributes.ActivityId)
	s.Equal(activity3Type, activity3Attributes.ActivityType.Name)
	s.Equal(int64(12), activity3Attributes.DecisionTaskCompletedEventId)
	s.Equal(tl, activity3Attributes.TaskList.Name)
	s.Equal(activity3Input, activity3Attributes.Input)
	s.Equal(int32(100), activity3Attributes.ScheduleToCloseTimeoutSeconds)
	s.Equal(int32(10), activity3Attributes.ScheduleToStartTimeoutSeconds)
	s.Equal(int32(50), activity3Attributes.StartToCloseTimeoutSeconds)
	s.Equal(int32(5), activity3Attributes.HeartbeatTimeoutSeconds)

	di, ok := executionBuilder.GetDecisionInfo(15)
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
}

func (s *engineSuite) TestValidateSignalRequest() {
	workflowType := "testType"
	input := payload.EncodeString("input")
	startRequest := &workflowservice.StartWorkflowExecutionRequest{
		WorkflowId:                          "ID",
		WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
		TaskList:                            &tasklistpb.TaskList{Name: "taskptr"},
		Input:                               input,
		ExecutionStartToCloseTimeoutSeconds: 10,
		TaskStartToCloseTimeoutSeconds:      10,
		Identity:                            "identity",
	}
	err := validateStartWorkflowExecutionRequest(startRequest, 999)
	s.Error(err, "startRequest doesn't have request id, it should error out")
}

func (s *engineSuite) TestRespondDecisionTaskCompletedMaxAttemptsExceeded() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	input := payload.EncodeString("input")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_ScheduleActivityTask,
		Attributes: &decisionpb.Decision_ScheduleActivityTaskDecisionAttributes{ScheduleActivityTaskDecisionAttributes: &decisionpb.ScheduleActivityTaskDecisionAttributes{
			ActivityId:                    "activity1",
			ActivityType:                  &commonpb.ActivityType{Name: "activity_type1"},
			TaskList:                      &tasklistpb.TaskList{Name: tl},
			Input:                         input,
			ScheduleToCloseTimeoutSeconds: 100,
			ScheduleToStartTimeoutSeconds: 10,
			StartToCloseTimeoutSeconds:    50,
			HeartbeatTimeoutSeconds:       5,
		}},
	}}

	for i := 0; i < conditionalRetryCount; i++ {
		ms := createMutableState(msBuilder)
		gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
		s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
		s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}},
			&persistence.ConditionFailedError{}).Once()
	}

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.Equal(ErrMaxAttemptsExceeded, err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedCompleteWorkflowFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	activity1Result := payload.EncodeString("activity1_result")
	activity2ID := "activity2"
	activity2Type := "activity_type2"
	activity2Input := payload.EncodeString("input2")
	activity2Result := payload.EncodeString("activity2_result")
	workflowResult := payload.EncodeString("workflow result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 25, 200, identity)
	di1 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di1.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di1.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId,
		activity1ID, activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity2ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId,
		activity2ID, activity2Type, tl, activity2Input, 100, 10, 1, 5)
	activity1StartedEvent := addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	activity2StartedEvent := addActivityTaskStartedEvent(msBuilder, activity2ScheduledEvent.EventId, identity)
	addActivityTaskCompletedEvent(msBuilder, activity1ScheduledEvent.EventId,
		activity1StartedEvent.EventId, activity1Result, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)
	addActivityTaskCompletedEvent(msBuilder, activity2ScheduledEvent.EventId,
		activity2StartedEvent.EventId, activity2Result, identity)

	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: di2.ScheduleID,
	}
	taskToken, _ := tt.Marshal()

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CompleteWorkflowExecution,
		Attributes: &decisionpb.Decision_CompleteWorkflowExecutionDecisionAttributes{CompleteWorkflowExecutionDecisionAttributes: &decisionpb.CompleteWorkflowExecutionDecisionAttributes{
			Result: workflowResult,
		}},
	}}

	for i := 0; i < 2; i++ {
		ms := createMutableState(msBuilder)
		gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	}

	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(15), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(decisionStartedEvent1.EventId, executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
	di3, ok := executionBuilder.GetDecisionInfo(executionBuilder.GetExecutionInfo().NextEventID - 1)
	s.True(ok)
	s.Equal(executionBuilder.GetExecutionInfo().NextEventID-1, di3.ScheduleID)
	s.Equal(int64(0), di3.Attempt)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedFailWorkflowFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	activity1Result := payload.EncodeString("activity1_result")
	activity2ID := "activity2"
	activity2Type := "activity_type2"
	activity2Input := payload.EncodeString("input2")
	activity2Result := payload.EncodeString("activity2_result")
	reason := "workflow fail reason"
	details := payload.EncodeString("workflow fail details")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 25, 200, identity)
	di1 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di1.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di1.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity1ID,
		activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity2ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity2ID,
		activity2Type, tl, activity2Input, 100, 10, 1, 5)
	activity1StartedEvent := addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	activity2StartedEvent := addActivityTaskStartedEvent(msBuilder, activity2ScheduledEvent.EventId, identity)
	addActivityTaskCompletedEvent(msBuilder, activity1ScheduledEvent.EventId,
		activity1StartedEvent.EventId, activity1Result, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)
	addActivityTaskCompletedEvent(msBuilder, activity2ScheduledEvent.EventId,
		activity2StartedEvent.EventId, activity2Result, identity)

	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: di2.ScheduleID,
	}
	taskToken, _ := tt.Marshal()

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_FailWorkflowExecution,
		Attributes: &decisionpb.Decision_FailWorkflowExecutionDecisionAttributes{FailWorkflowExecutionDecisionAttributes: &decisionpb.FailWorkflowExecutionDecisionAttributes{
			Reason:  reason,
			Details: details,
		}},
	}}

	for i := 0; i < 2; i++ {
		ms := createMutableState(msBuilder)
		gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	}

	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(15), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(decisionStartedEvent1.EventId, executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
	di3, ok := executionBuilder.GetDecisionInfo(executionBuilder.GetExecutionInfo().NextEventID - 1)
	s.True(ok)
	s.Equal(executionBuilder.GetExecutionInfo().NextEventID-1, di3.ScheduleID)
	s.Equal(int64(0), di3.Attempt)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedBadDecisionAttributes() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	activity1Result := payload.EncodeString("activity1_result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 25, 200, identity)
	di1 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di1.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di1.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity1ID,
		activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity1StartedEvent := addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	addActivityTaskCompletedEvent(msBuilder, activity1ScheduledEvent.EventId,
		activity1StartedEvent.EventId, activity1Result, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: di2.ScheduleID,
	}
	taskToken, _ := tt.Marshal()

	// Decision with nil attributes
	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CompleteWorkflowExecution,
	}}

	gwmsResponse1 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse1, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()

	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{
		MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil,
	).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)
}

// This test unit tests the activity schedule timeout validation logic of HistoryEngine's RespondDecisionTaskComplete function.
// An scheduled activity decision has 3 timeouts: ScheduleToClose, ScheduleToStart and StartToClose.
// This test verifies that when either ScheduleToClose or ScheduleToStart and StartToClose are specified,
// HistoryEngine's validateActivityScheduleAttribute will deduce the missing timeout and fill it in
// instead of returning a BadRequest error and only when all three are missing should a BadRequest be returned.
func (s *engineSuite) TestRespondDecisionTaskCompletedSingleActivityScheduledAttribute() {
	workflowTimeout := int32(100)
	testIterationVariables := []struct {
		scheduleToClose         int32
		scheduleToStart         int32
		startToClose            int32
		heartbeat               int32
		expectedScheduleToClose int32
		expectedScheduleToStart int32
		expectedStartToClose    int32
		expectDecisionFail      bool
	}{
		// No ScheduleToClose timeout, will use ScheduleToStart + StartToClose
		{0, 3, 7, 0,
			3 + 7, 3, 7, false},
		// Has ScheduleToClose timeout but not ScheduleToStart or StartToClose,
		// will use ScheduleToClose for ScheduleToStart and StartToClose
		{7, 0, 0, 0,
			7, 7, 7, false},
		// Only StartToClose timeout
		{0, 0, 7, 0,
			workflowTimeout, workflowTimeout, 7, false},
		// No ScheduleToClose timeout, ScheduleToStart or StartToClose, expect error return
		{0, 0, 0, 0,
			0, 0, 0, true},
		// Negative ScheduleToClose, expect error return
		{-1, 0, 0, 0,
			0, 0, 0, true},
		// Negative ScheduleToStart, expect error return
		{0, -1, 0, 0,
			0, 0, 0, true},
		// Negative StartToClose, expect error return
		{0, 0, -1, 0,
			0, 0, 0, true},
		// Negative HeartBeat, expect error return
		{0, 0, 0, -1,
			0, 0, 0, true},
		// Use workflow timeout
		{workflowTimeout, 0, 0, 0,
			workflowTimeout, workflowTimeout, workflowTimeout, false},
		// Timeout larger than workflow timeout
		{workflowTimeout + 1, 0, 0, 0,
			workflowTimeout, workflowTimeout, workflowTimeout, false},
		{0, workflowTimeout + 1, 0, 0,
			0, 0, 0, true},
		{0, 0, workflowTimeout + 1, 0,
			workflowTimeout, workflowTimeout, workflowTimeout, false},
		{0, 0, 0, workflowTimeout + 1,
			0, 0, 0, true},
		// No ScheduleToClose timeout, will use ScheduleToStart + StartToClose, but exceed limit
		{0, workflowTimeout, 10, 0,
			workflowTimeout, workflowTimeout, 10, false},
	}

	for _, iVar := range testIterationVariables {
		we := executionpb.WorkflowExecution{
			WorkflowId: "wId",
			RunId:      testRunID,
		}
		tl := "testTaskList"
		tt := &tokengenpb.Task{
			WorkflowId: "wId",
			RunId:      primitives.MustParseUUID(we.GetRunId()),
			ScheduleId: 2,
		}
		taskToken, _ := tt.Marshal()
		identity := "testIdentity"
		input := payload.EncodeString("input")

		msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
			loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
		addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), workflowTimeout, 200, identity)
		di := addDecisionTaskScheduledEvent(msBuilder)
		addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

		decisions := []*decisionpb.Decision{{
			DecisionType: decisionpb.DecisionType_ScheduleActivityTask,
			Attributes: &decisionpb.Decision_ScheduleActivityTaskDecisionAttributes{ScheduleActivityTaskDecisionAttributes: &decisionpb.ScheduleActivityTaskDecisionAttributes{
				ActivityId:                    "activity1",
				ActivityType:                  &commonpb.ActivityType{Name: "activity_type1"},
				TaskList:                      &tasklistpb.TaskList{Name: tl},
				Input:                         input,
				ScheduleToCloseTimeoutSeconds: iVar.scheduleToClose,
				ScheduleToStartTimeoutSeconds: iVar.scheduleToStart,
				StartToCloseTimeoutSeconds:    iVar.startToClose,
				HeartbeatTimeoutSeconds:       iVar.heartbeat,
			}},
		}}

		gwmsResponse1 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}
		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse1, nil).Once()
		if iVar.expectDecisionFail {
			gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}
			s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
		}

		s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
		s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

		_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
			NamespaceId: testNamespaceID,
			CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
				TaskToken: taskToken,
				Decisions: decisions,
				Identity:  identity,
			},
		})

		s.Nil(err, s.printHistory(msBuilder))
		executionBuilder := s.getBuilder(testNamespaceID, we)
		if !iVar.expectDecisionFail {
			s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
			s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
			s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
			s.False(executionBuilder.HasPendingDecision())

			activity1Attributes := s.getActivityScheduledEvent(executionBuilder, int64(5)).GetActivityTaskScheduledEventAttributes()
			s.Equal(iVar.expectedScheduleToClose, activity1Attributes.GetScheduleToCloseTimeoutSeconds())
			s.Equal(iVar.expectedScheduleToStart, activity1Attributes.GetScheduleToStartTimeoutSeconds())
			s.Equal(iVar.expectedStartToClose, activity1Attributes.GetStartToCloseTimeoutSeconds())
		} else {
			s.Equal(int64(5), executionBuilder.GetExecutionInfo().NextEventID)
			s.Equal(common.EmptyEventID, executionBuilder.GetExecutionInfo().LastProcessedEvent)
			s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
			s.True(executionBuilder.HasPendingDecision())
		}
		s.TearDownTest()
		s.SetupTest()
	}
}

func (s *engineSuite) TestRespondDecisionTaskCompletedBadBinary() {
	namespaceID := uuid.New()
	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	namespaceEntry := cache.NewLocalNamespaceCacheEntryForTest(
		&persistenceblobs.NamespaceInfo{Id: primitives.MustParseUUID(namespaceID), Name: testNamespace},
		&persistenceblobs.NamespaceConfig{
			RetentionDays: 2,
			BadBinaries: &namespacepb.BadBinaries{
				Binaries: map[string]*namespacepb.BadBinaryInfo{
					"test-bad-binary": {},
				},
			},
		},
		cluster.TestCurrentClusterName,
		nil,
	)

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	msBuilder.namespaceEntry = namespaceEntry
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	var decisions []*decisionpb.Decision

	gwmsResponse1 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: createMutableState(msBuilder)}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse1, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()
	s.mockNamespaceCache.EXPECT().GetNamespaceByID(namespaceID).Return(namespaceEntry, nil).AnyTimes()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: namespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken:      taskToken,
			Decisions:      decisions,
			Identity:       identity,
			BinaryChecksum: "test-bad-binary",
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(namespaceID, we)
	s.Equal(int64(5), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(common.EmptyEventID, executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRespondDecisionTaskCompletedSingleActivityScheduledDecision() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	input := payload.EncodeString("input")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_ScheduleActivityTask,
		Attributes: &decisionpb.Decision_ScheduleActivityTaskDecisionAttributes{ScheduleActivityTaskDecisionAttributes: &decisionpb.ScheduleActivityTaskDecisionAttributes{
			ActivityId:                    "activity1",
			ActivityType:                  &commonpb.ActivityType{Name: "activity_type1"},
			TaskList:                      &tasklistpb.TaskList{Name: tl},
			Input:                         input,
			ScheduleToCloseTimeoutSeconds: 100,
			ScheduleToStartTimeoutSeconds: 10,
			StartToCloseTimeoutSeconds:    50,
			HeartbeatTimeoutSeconds:       5,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())

	activity1Attributes := s.getActivityScheduledEvent(executionBuilder, int64(5)).GetActivityTaskScheduledEventAttributes()
	s.Equal("activity1", activity1Attributes.ActivityId)
	s.Equal("activity_type1", activity1Attributes.ActivityType.Name)
	s.Equal(int64(4), activity1Attributes.DecisionTaskCompletedEventId)
	s.Equal(tl, activity1Attributes.TaskList.Name)
	s.Equal(input, activity1Attributes.Input)
	s.Equal(int32(100), activity1Attributes.ScheduleToCloseTimeoutSeconds)
	s.Equal(int32(10), activity1Attributes.ScheduleToStartTimeoutSeconds)
	s.Equal(int32(50), activity1Attributes.StartToCloseTimeoutSeconds)
	s.Equal(int32(5), activity1Attributes.HeartbeatTimeoutSeconds)
}

func (s *engineSuite) TestRespondDecisionTaskCompleted_DecisionHeartbeatTimeout() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	msBuilder.executionInfo.DecisionOriginalScheduledTimestamp = time.Now().Add(-time.Hour).UnixNano()

	decisions := []*decisionpb.Decision{}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			ForceCreateNewDecisionTask: true,
			TaskToken:                  taskToken,
			Decisions:                  decisions,
			Identity:                   identity,
		},
	})
	s.Error(err, "decision heartbeat timeout")
}

func (s *engineSuite) TestRespondDecisionTaskCompleted_DecisionHeartbeatNotTimeout() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	msBuilder.executionInfo.DecisionOriginalScheduledTimestamp = time.Now().Add(-time.Minute).UnixNano()

	decisions := []*decisionpb.Decision{}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			ForceCreateNewDecisionTask: true,
			TaskToken:                  taskToken,
			Decisions:                  decisions,
			Identity:                   identity,
		},
	})
	s.Nil(err)
}

func (s *engineSuite) TestRespondDecisionTaskCompleted_DecisionHeartbeatNotTimeout_ZeroOrignalScheduledTime() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	msBuilder.executionInfo.DecisionOriginalScheduledTimestamp = 0

	decisions := []*decisionpb.Decision{}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			ForceCreateNewDecisionTask: true,
			TaskToken:                  taskToken,
			Decisions:                  decisions,
			Identity:                   identity,
		},
	})
	s.Nil(err)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedCompleteWorkflowSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	workflowResult := payload.EncodeString("success")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CompleteWorkflowExecution,
		Attributes: &decisionpb.Decision_CompleteWorkflowExecutionDecisionAttributes{CompleteWorkflowExecutionDecisionAttributes: &decisionpb.CompleteWorkflowExecutionDecisionAttributes{
			Result: workflowResult,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateCompleted, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRespondDecisionTaskCompletedFailWorkflowSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	details := payload.EncodeString("fail workflow details")
	reason := "fail workflow reason"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_FailWorkflowExecution,
		Attributes: &decisionpb.Decision_FailWorkflowExecutionDecisionAttributes{FailWorkflowExecutionDecisionAttributes: &decisionpb.FailWorkflowExecutionDecisionAttributes{
			Reason:  reason,
			Details: details,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateCompleted, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRespondDecisionTaskCompletedSignalExternalWorkflowSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_SignalExternalWorkflowExecution,
		Attributes: &decisionpb.Decision_SignalExternalWorkflowExecutionDecisionAttributes{SignalExternalWorkflowExecutionDecisionAttributes: &decisionpb.SignalExternalWorkflowExecutionDecisionAttributes{
			Namespace: testNamespace,
			Execution: &executionpb.WorkflowExecution{
				WorkflowId: we.WorkflowId,
				RunId:      we.RunId,
			},
			SignalName: "signal",
			Input:      payload.EncodeString("test input"),
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
}

func (s *engineSuite) TestRespondDecisionTaskCompletedStartChildWorkflowWithAbandonPolicy() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	abandon := commonpb.ParentClosePolicy_Abandon
	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_StartChildWorkflowExecution,
		Attributes: &decisionpb.Decision_StartChildWorkflowExecutionDecisionAttributes{StartChildWorkflowExecutionDecisionAttributes: &decisionpb.StartChildWorkflowExecutionDecisionAttributes{
			Namespace:  testNamespace,
			WorkflowId: "child-workflow-id",
			WorkflowType: &commonpb.WorkflowType{
				Name: "child-workflow-type",
			},
			ParentClosePolicy: abandon,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(int(1), len(executionBuilder.GetPendingChildExecutionInfos()))
	var childID int64
	for c := range executionBuilder.GetPendingChildExecutionInfos() {
		childID = c
		break
	}
	s.Equal("child-workflow-id", executionBuilder.GetPendingChildExecutionInfos()[childID].StartedWorkflowID)
	s.Equal(commonpb.ParentClosePolicy_Abandon, commonpb.ParentClosePolicy(executionBuilder.GetPendingChildExecutionInfos()[childID].ParentClosePolicy))
}

func (s *engineSuite) TestRespondDecisionTaskCompletedStartChildWorkflowWithTerminatePolicy() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	terminate := commonpb.ParentClosePolicy_Terminate
	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_StartChildWorkflowExecution,
		Attributes: &decisionpb.Decision_StartChildWorkflowExecutionDecisionAttributes{StartChildWorkflowExecutionDecisionAttributes: &decisionpb.StartChildWorkflowExecutionDecisionAttributes{
			Namespace:  testNamespace,
			WorkflowId: "child-workflow-id",
			WorkflowType: &commonpb.WorkflowType{
				Name: "child-workflow-type",
			},
			ParentClosePolicy: terminate,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(int(1), len(executionBuilder.GetPendingChildExecutionInfos()))
	var childID int64
	for c := range executionBuilder.GetPendingChildExecutionInfos() {
		childID = c
		break
	}
	s.Equal("child-workflow-id", executionBuilder.GetPendingChildExecutionInfos()[childID].StartedWorkflowID)
	s.Equal(commonpb.ParentClosePolicy_Terminate, commonpb.ParentClosePolicy(executionBuilder.GetPendingChildExecutionInfos()[childID].ParentClosePolicy))
}

// RunID Invalid is no longer possible form this scope.
/*func (s *engineSuite) TestRespondDecisionTaskCompletedSignalExternalWorkflowFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      "invalid run id",
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.GetWorkflowId(),
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 2,
	}
                                  taskToken, _  := tt.Marshal()
	identity := "testIdentity"
	executionContext := []byte("context")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, codec.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_SignalExternalWorkflowExecution,
		Attributes: &decisionpb.Decision_SignalExternalWorkflowExecutionDecisionAttributes{SignalExternalWorkflowExecutionDecisionAttributes: &decisionpb.SignalExternalWorkflowExecutionDecisionAttributes{
			Namespace: testNamespaceID,
			Execution: &executionpb.WorkflowExecution{
				WorkflowId: we.WorkflowId,
				RunId:      we.RunId,
			},
			SignalName: "signal",
			Input:      codec.EncodeString("test input"),
		}},
	}}

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			Task:        taskToken,
			Decisions:        decisions,
			ExecutionContext: executionContext,
			Identity:         identity,
		},
	})

	s.EqualError(err, "RunID is not valid UUID.")
}*/

func (s *engineSuite) TestRespondDecisionTaskCompletedSignalExternalWorkflowFailed_UnKnownNamespace() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	foreignNamespace := "unknown namespace"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_SignalExternalWorkflowExecution,
		Attributes: &decisionpb.Decision_SignalExternalWorkflowExecutionDecisionAttributes{SignalExternalWorkflowExecutionDecisionAttributes: &decisionpb.SignalExternalWorkflowExecutionDecisionAttributes{
			Namespace: foreignNamespace,
			Execution: &executionpb.WorkflowExecution{
				WorkflowId: we.WorkflowId,
				RunId:      we.RunId,
			},
			SignalName: "signal",
			Input:      payload.EncodeString("test input"),
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockNamespaceCache.EXPECT().GetNamespace(foreignNamespace).Return(
		nil, errors.New("get foreign namespace error"),
	).Times(1)

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})

	s.NotNil(err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedInvalidToken() {

	invalidToken, _ := json.Marshal("bad token")
	identity := "testIdentity"

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: invalidToken,
			Result:    nil,
			Identity:  identity,
		},
	})

	s.NotNil(err)
	s.IsType(&serviceerror.InvalidArgument{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfNoExecution() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, serviceerror.NewNotFound("")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfNoRunID() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(nil, serviceerror.NewNotFound("")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfGetExecutionFailed() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, errors.New("FAILED")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfNoAIdProvided() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
	}
	taskToken, _ := tt.Marshal()

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "Neither ActivityID nor ScheduleID is provided")
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfNotFound() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
		ActivityId: "aid",
	}
	taskToken, _ := tt.Marshal()
	execution := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.Error(err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedUpdateExecutionFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, errors.New("FAILED")).Once()
	s.mockShardManager.On("UpdateShard", mock.Anything).Return(nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfTaskCompleted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	activityStartedEvent := addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	addActivityTaskCompletedEvent(msBuilder, activityScheduledEvent.EventId, activityStartedEvent.EventId,
		activityResult, identity)
	addDecisionTaskScheduledEvent(msBuilder)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedIfTaskNotStarted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 200, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedConflictOnUpdate() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	activity1Result := payload.EncodeString("activity1_result")
	activity2ID := "activity2"
	activity2Type := "activity_type2"
	activity2Input := payload.EncodeString("input2")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity1ID,
		activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity2ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity2ID,
		activity2Type, tl, activity2Input, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	addActivityTaskStartedEvent(msBuilder, activity2ScheduledEvent.EventId, identity)

	ms1 := createMutableState(msBuilder)
	gwmsResponse1 := &persistence.GetWorkflowExecutionResponse{State: ms1}

	ms2 := createMutableState(msBuilder)
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: ms2}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse1, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, &persistence.ConditionFailedError{}).Once()

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activity1Result,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(10))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(10), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskCompletedMaxAttemptsExceeded() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	for i := 0; i < conditionalRetryCount; i++ {
		ms := createMutableState(msBuilder)
		gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
		s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
		s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, &persistence.ConditionFailedError{}).Once()
	}

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.Equal(ErrMaxAttemptsExceeded, err)
}

func (s *engineSuite) TestRespondActivityTaskCompletedSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(9), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(8))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(8), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskCompletedByIdSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"

	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	activityResult := payload.EncodeString("activity result")
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		ScheduleId: common.EmptyEventID,
		ActivityId: activityID,
	}
	taskToken, _ := tt.Marshal()

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	decisionScheduledEvent := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, decisionScheduledEvent.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, decisionScheduledEvent.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: we.RunId}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCompleted(context.Background(), &historyservice.RespondActivityTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondActivityTaskCompletedRequest{
			TaskToken: taskToken,
			Result:    activityResult,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(9), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(8))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(8), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskFailedInvalidToken() {

	invalidToken, _ := json.Marshal("bad token")
	identity := "testIdentity"

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: invalidToken,
			Identity:  identity,
		},
	})

	s.NotNil(err)
	s.IsType(&serviceerror.InvalidArgument{}, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedIfNoExecution() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil,
		serviceerror.NewNotFound("")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedIfNoRunID() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(nil,
		serviceerror.NewNotFound("")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedIfGetExecutionFailed() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(testRunID),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil,
		errors.New("FAILED")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondActivityTaskFailededIfNoAIdProvided() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
	}
	taskToken, _ := tt.Marshal()
	execution := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "Neither ActivityID nor ScheduleID is provided")
}

func (s *engineSuite) TestRespondActivityTaskFailededIfNotFound() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
		ActivityId: "aid",
	}
	taskToken, _ := tt.Marshal()
	execution := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.Error(err)
}

func (s *engineSuite) TestRespondActivityTaskFailedUpdateExecutionFailed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, errors.New("FAILED")).Once()
	s.mockShardManager.On("UpdateShard", mock.Anything).Return(nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "FAILED")
}

func (s *engineSuite) TestRespondActivityTaskFailedIfTaskCompleted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	failReason := "fail reason"
	details := payload.EncodeString("fail details")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	activityStartedEvent := addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	addActivityTaskFailedEvent(msBuilder, activityScheduledEvent.EventId, activityStartedEvent.EventId,
		failReason, details, identity)
	addDecisionTaskScheduledEvent(msBuilder)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Reason:    failReason,
			Details:   details,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedIfTaskNotStarted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedConflictOnUpdate() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activity1ID := "activity1"
	activity1Type := "activity_type1"
	activity1Input := payload.EncodeString("input1")
	failReason := "fail reason"
	details := payload.EncodeString("fail details.")
	activity2ID := "activity2"
	activity2Type := "activity_type2"
	activity2Input := payload.EncodeString("input2")
	activity2Result := payload.EncodeString("activity2_result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 25, 25, identity)
	di1 := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent1 := addDecisionTaskStartedEvent(msBuilder, di1.ScheduleID, tl, identity)
	decisionCompletedEvent1 := addDecisionTaskCompletedEvent(msBuilder, di1.ScheduleID, decisionStartedEvent1.EventId, identity)
	activity1ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity1ID,
		activity1Type, tl, activity1Input, 100, 10, 1, 5)
	activity2ScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent1.EventId, activity2ID,
		activity2Type, tl, activity2Input, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activity1ScheduledEvent.EventId, identity)
	activity2StartedEvent := addActivityTaskStartedEvent(msBuilder, activity2ScheduledEvent.EventId, identity)

	ms1 := createMutableState(msBuilder)
	gwmsResponse1 := &persistence.GetWorkflowExecutionResponse{State: ms1}

	addActivityTaskCompletedEvent(msBuilder, activity2ScheduledEvent.EventId,
		activity2StartedEvent.EventId, activity2Result, identity)
	addDecisionTaskScheduledEvent(msBuilder)

	ms2 := createMutableState(msBuilder)
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: ms2}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse1, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, &persistence.ConditionFailedError{}).Once()

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Reason:    failReason,
			Details:   details,
			Identity:  identity,
		},
	})
	s.Nil(err, s.printHistory(msBuilder))
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(12), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(10))
	s.True(ok)
	s.Equal(int32(25), di.DecisionTimeout)
	s.Equal(int64(10), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskFailedMaxAttemptsExceeded() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	for i := 0; i < conditionalRetryCount; i++ {
		ms := createMutableState(msBuilder)
		gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
		s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
		s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, &persistence.ConditionFailedError{}).Once()
	}

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.Equal(ErrMaxAttemptsExceeded, err)
}

func (s *engineSuite) TestRespondActivityTaskFailedSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	failReason := "failed"
	failDetails := payload.EncodeString("fail details.")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Reason:    failReason,
			Details:   failDetails,
			Identity:  identity,
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(9), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(8))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(8), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskFailedByIdSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"

	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	failReason := "failed"
	failDetails := payload.EncodeString("fail details.")
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		ScheduleId: common.EmptyEventID,
		ActivityId: activityID,
	}
	taskToken, _ := tt.Marshal()

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	decisionScheduledEvent := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, decisionScheduledEvent.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, decisionScheduledEvent.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 5)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: we.RunId}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskFailed(context.Background(), &historyservice.RespondActivityTaskFailedRequest{
		NamespaceId: testNamespaceID,
		FailedRequest: &workflowservice.RespondActivityTaskFailedRequest{
			TaskToken: taskToken,
			Reason:    failReason,
			Details:   failDetails,
			Identity:  identity,
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(9), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(8))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(8), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRecordActivityTaskHeartBeatSuccess_NoTimer() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 0)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	// No HeartBeat timer running.
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	detais := payload.EncodeString("details")

	_, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   detais,
		},
	})
	s.Nil(err)
}

func (s *engineSuite) TestRecordActivityTaskHeartBeatSuccess_TimerRunning() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	// HeartBeat timer running.
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	detais := payload.EncodeString("details")

	_, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   detais,
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRecordActivityTaskHeartBeatByIDSuccess() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: common.EmptyEventID,
		ActivityId: activityID,
	}
	taskToken, _ := tt.Marshal()

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 0)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)

	// No HeartBeat timer running.
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	detais := payload.EncodeString("details")

	_, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   detais,
		},
	})
	s.Nil(err)
}

func (s *engineSuite) TestRespondActivityTaskCanceled_Scheduled() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCanceled_Started() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 5,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	_, _, err := msBuilder.AddActivityTaskCancelRequestedEvent(decisionCompletedEvent.EventId, activityID, identity)
	s.Nil(err)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(10), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(9))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(9), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskCanceledById_Started() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		ScheduleId: common.EmptyEventID,
		ActivityId: activityID,
	}
	taskToken, _ := tt.Marshal()

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	decisionScheduledEvent := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, decisionScheduledEvent.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, decisionScheduledEvent.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	_, _, err := msBuilder.AddActivityTaskCancelRequestedEvent(decisionCompletedEvent.EventId, activityID, identity)
	s.Nil(err)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: we.RunId}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(10), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	s.True(executionBuilder.HasPendingDecision())
	di, ok := executionBuilder.GetDecisionInfo(int64(9))
	s.True(ok)
	s.Equal(int32(100), di.DecisionTimeout)
	s.Equal(int64(9), di.ScheduleID)
	s.Equal(common.EmptyEventID, di.StartedID)
}

func (s *engineSuite) TestRespondActivityTaskCanceledIfNoRunID() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(nil, serviceerror.NewNotFound("")).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.NotNil(err)
	s.IsType(&serviceerror.NotFound{}, err)
}

func (s *engineSuite) TestRespondActivityTaskCanceledIfNoAIdProvided() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.EqualError(err, "Neither ActivityID nor ScheduleID is provided")
}

func (s *engineSuite) TestRespondActivityTaskCanceledIfNotFound() {

	tt := &tokengenpb.Task{
		WorkflowId: "wId",
		ScheduleId: common.EmptyEventID,
		ActivityId: "aid",
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	gceResponse := &persistence.GetCurrentExecutionResponse{RunID: testRunID}

	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gceResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err := s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: taskToken,
			Identity:  identity,
		},
	})
	s.Error(err)
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_NotScheduled() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)
	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_Scheduled() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 6,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(12), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
	di2, ok := executionBuilder.GetDecisionInfo(executionBuilder.GetExecutionInfo().NextEventID - 1)
	s.True(ok)
	s.Equal(executionBuilder.GetExecutionInfo().NextEventID-1, di2.ScheduleID)
	s.Equal(int64(0), di2.Attempt)
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_Started() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 7,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 0)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_Completed() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 6,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")
	workflowResult := payload.EncodeString("workflow result")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 0)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	decisions := []*decisionpb.Decision{
		{
			DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
			Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
				ActivityId: activityID,
			}},
		},
		{
			DecisionType: decisionpb.DecisionType_CompleteWorkflowExecution,
			Attributes: &decisionpb.Decision_CompleteWorkflowExecutionDecisionAttributes{CompleteWorkflowExecutionDecisionAttributes: &decisionpb.CompleteWorkflowExecutionDecisionAttributes{
				Result: workflowResult,
			}},
		},
	}

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateCompleted, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_NoHeartBeat() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 7,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 0)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())

	// Try recording activity heartbeat
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	att := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 5,
	}
	activityTaskToken, _ := att.Marshal()

	hbResponse, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)
	s.NotNil(hbResponse)
	s.True(hbResponse.CancelRequested)

	// Try cancelling the request.
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)

	executionBuilder = s.getBuilder(testNamespaceID, we)
	s.Equal(int64(13), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_Success() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 7,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())

	// Try recording activity heartbeat
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	att := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 5,
	}
	activityTaskToken, _ := att.Marshal()

	hbResponse, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)
	s.NotNil(hbResponse)
	s.True(hbResponse.CancelRequested)

	// Try cancelling the request.
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)

	executionBuilder = s.getBuilder(testNamespaceID, we)
	s.Equal(int64(13), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestRequestCancel_RespondDecisionTaskCompleted_SuccessWithQueries() {
	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 7,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	activityID := "activity1_id"
	activityType := "activity_type1"
	activityInput := payload.EncodeString("input1")

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	activityScheduledEvent, _ := addActivityTaskScheduledEvent(msBuilder, decisionCompletedEvent.EventId, activityID,
		activityType, tl, activityInput, 100, 10, 1, 1)
	addActivityTaskStartedEvent(msBuilder, activityScheduledEvent.EventId, identity)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_RequestCancelActivityTask,
		Attributes: &decisionpb.Decision_RequestCancelActivityTaskDecisionAttributes{RequestCancelActivityTaskDecisionAttributes: &decisionpb.RequestCancelActivityTaskDecisionAttributes{
			ActivityId: activityID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	// load mutable state such that it already exists in memory when respond decision task is called
	// this enables us to set query registry on it
	ctx, release, err := s.mockHistoryEngine.historyCache.getOrCreateWorkflowExecutionForBackground(testNamespaceID, we)
	s.NoError(err)
	loadedMS, err := ctx.loadWorkflowExecution()
	s.NoError(err)
	qr := newQueryRegistry()
	id1, _ := qr.bufferQuery(&querypb.WorkflowQuery{})
	id2, _ := qr.bufferQuery(&querypb.WorkflowQuery{})
	id3, _ := qr.bufferQuery(&querypb.WorkflowQuery{})
	loadedMS.(*mutableStateBuilder).queryRegistry = qr
	release(nil)
	result1 := &querypb.WorkflowQueryResult{
		ResultType: querypb.QueryResultType_Answered,
		Answer:     payload.EncodeBytes([]byte{1, 2, 3}),
	}
	result2 := &querypb.WorkflowQueryResult{
		ResultType:   querypb.QueryResultType_Failed,
		ErrorMessage: "error reason",
	}
	queryResults := map[string]*querypb.WorkflowQueryResult{
		id1: result1,
		id2: result2,
	}
	_, err = s.mockHistoryEngine.RespondDecisionTaskCompleted(s.constructCallContext(headers.SupportedGoSDKVersion), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken:    taskToken,
			Decisions:    decisions,
			Identity:     identity,
			QueryResults: queryResults,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(11), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
	s.Len(qr.getCompletedIDs(), 2)
	completed1, err := qr.getTerminationState(id1)
	s.NoError(err)
	s.EqualValues(completed1.queryResult, result1)
	s.Equal(queryTerminationTypeCompleted, completed1.queryTerminationType)
	completed2, err := qr.getTerminationState(id2)
	s.NoError(err)
	s.EqualValues(completed2.queryResult, result2)
	s.Equal(queryTerminationTypeCompleted, completed2.queryTerminationType)
	s.Len(qr.getBufferedIDs(), 0)
	s.Len(qr.getFailedIDs(), 0)
	s.Len(qr.getUnblockedIDs(), 1)
	unblocked1, err := qr.getTerminationState(id3)
	s.NoError(err)
	s.Nil(unblocked1.queryResult)
	s.Equal(queryTerminationTypeUnblocked, unblocked1.queryTerminationType)

	// Try recording activity heartbeat
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	att := &tokengenpb.Task{
		WorkflowId: "wId",
		RunId:      primitives.MustParseUUID(we.GetRunId()),
		ScheduleId: 5,
	}
	activityTaskToken, _ := att.Marshal()

	hbResponse, err := s.mockHistoryEngine.RecordActivityTaskHeartbeat(context.Background(), &historyservice.RecordActivityTaskHeartbeatRequest{
		NamespaceId: testNamespaceID,
		HeartbeatRequest: &workflowservice.RecordActivityTaskHeartbeatRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)
	s.NotNil(hbResponse)
	s.True(hbResponse.CancelRequested)

	// Try cancelling the request.
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RespondActivityTaskCanceled(context.Background(), &historyservice.RespondActivityTaskCanceledRequest{
		NamespaceId: testNamespaceID,
		CancelRequest: &workflowservice.RespondActivityTaskCanceledRequest{
			TaskToken: activityTaskToken,
			Identity:  identity,
			Details:   payload.EncodeString("details"),
		},
	})
	s.Nil(err)

	executionBuilder = s.getBuilder(testNamespaceID, we)
	s.Equal(int64(13), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(8), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) constructCallContext(featureVersion string) context.Context {
	return headers.SetVersionsForTests(context.Background(), headers.SupportedGoSDKVersion, headers.GoSDK, featureVersion)
}

func (s *engineSuite) TestStarTimer_DuplicateTimerID() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	timerID := "t1"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())

	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_StartTimer,
		Attributes: &decisionpb.Decision_StartTimerDecisionAttributes{StartTimerDecisionAttributes: &decisionpb.StartTimerDecisionAttributes{
			TimerId:                   timerID,
			StartToFireTimeoutSeconds: 1,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)

	// Try to add the same timer ID again.
	di2 := addDecisionTaskScheduledEvent(executionBuilder)
	addDecisionTaskStartedEvent(executionBuilder, di2.ScheduleID, tl, identity)
	tt2 := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: di2.ScheduleID,
	}
	taskToken2, _ := tt2.Marshal()

	ms2 := createMutableState(executionBuilder)
	gwmsResponse2 := &persistence.GetWorkflowExecutionResponse{State: ms2}

	decisionFailedEvent := false
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse2, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Run(func(arguments mock.Arguments) {
		req := arguments.Get(0).(*persistence.AppendHistoryNodesRequest)
		decTaskIndex := len(req.Events) - 1
		if decTaskIndex >= 0 && req.Events[decTaskIndex].EventType == eventpb.EventType_DecisionTaskFailed {
			decisionFailedEvent = true
		}
	}).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err = s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken2,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	s.True(decisionFailedEvent)
	executionBuilder = s.getBuilder(testNamespaceID, we)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.True(executionBuilder.HasPendingDecision())
	di3, ok := executionBuilder.GetDecisionInfo(executionBuilder.GetExecutionInfo().NextEventID)
	s.True(ok, "DI.ScheduleID: %v, ScheduleID: %v, StartedID: %v", di2.ScheduleID,
		executionBuilder.GetExecutionInfo().DecisionScheduleID, executionBuilder.GetExecutionInfo().DecisionStartedID)
	s.Equal(executionBuilder.GetExecutionInfo().NextEventID, di3.ScheduleID)
	s.Equal(int64(1), di3.Attempt)
}

func (s *engineSuite) TestUserTimer_RespondDecisionTaskCompleted() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 6,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	timerID := "t1"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	// Verify cancel timer with a start event.
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addTimerStartedEvent(msBuilder, decisionCompletedEvent.EventId, timerID, 10)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CancelTimer,
		Attributes: &decisionpb.Decision_CancelTimerDecisionAttributes{CancelTimerDecisionAttributes: &decisionpb.CancelTimerDecisionAttributes{
			TimerId: timerID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(10), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestCancelTimer_RespondDecisionTaskCompleted_NoStartTimer() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 2,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	timerID := "t1"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	// Verify cancel timer with a start event.
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CancelTimer,
		Attributes: &decisionpb.Decision_CancelTimerDecisionAttributes{CancelTimerDecisionAttributes: &decisionpb.CancelTimerDecisionAttributes{
			TimerId: timerID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err := s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(6), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(3), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
}

func (s *engineSuite) TestCancelTimer_RespondDecisionTaskCompleted_TimerFired() {

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tl := "testTaskList"
	tt := &tokengenpb.Task{
		WorkflowId: we.WorkflowId,
		RunId:      primitives.MustParseUUID(we.RunId),
		ScheduleId: 6,
	}
	taskToken, _ := tt.Marshal()
	identity := "testIdentity"
	timerID := "t1"

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	// Verify cancel timer with a start event.
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tl, payload.EncodeString("input"), 100, 100, identity)
	di := addDecisionTaskScheduledEvent(msBuilder)
	decisionStartedEvent := addDecisionTaskStartedEvent(msBuilder, di.ScheduleID, tl, identity)
	decisionCompletedEvent := addDecisionTaskCompletedEvent(msBuilder, di.ScheduleID, decisionStartedEvent.EventId, identity)
	addTimerStartedEvent(msBuilder, decisionCompletedEvent.EventId, timerID, 10)
	di2 := addDecisionTaskScheduledEvent(msBuilder)
	addDecisionTaskStartedEvent(msBuilder, di2.ScheduleID, tl, identity)
	addTimerFiredEvent(msBuilder, timerID)
	_, _, err := msBuilder.CloseTransactionAsMutation(time.Now(), transactionPolicyActive)
	s.Nil(err)

	ms := createMutableState(msBuilder)
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
	s.True(len(gwmsResponse.State.BufferedEvents) > 0)

	decisions := []*decisionpb.Decision{{
		DecisionType: decisionpb.DecisionType_CancelTimer,
		Attributes: &decisionpb.Decision_CancelTimerDecisionAttributes{CancelTimerDecisionAttributes: &decisionpb.CancelTimerDecisionAttributes{
			TimerId: timerID,
		}},
	}}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.MatchedBy(func(input *persistence.UpdateWorkflowExecutionRequest) bool {
		// need to check whether the buffered events are cleared
		s.True(input.UpdateWorkflowMutation.ClearBufferedEvents)
		return true
	})).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	_, err = s.mockHistoryEngine.RespondDecisionTaskCompleted(context.Background(), &historyservice.RespondDecisionTaskCompletedRequest{
		NamespaceId: testNamespaceID,
		CompleteRequest: &workflowservice.RespondDecisionTaskCompletedRequest{
			TaskToken: taskToken,
			Decisions: decisions,
			Identity:  identity,
		},
	})
	s.Nil(err)

	executionBuilder := s.getBuilder(testNamespaceID, we)
	s.Equal(int64(10), executionBuilder.GetExecutionInfo().NextEventID)
	s.Equal(int64(7), executionBuilder.GetExecutionInfo().LastProcessedEvent)
	s.Equal(persistence.WorkflowStateRunning, executionBuilder.GetExecutionInfo().State)
	s.False(executionBuilder.HasPendingDecision())
	s.False(executionBuilder.HasBufferedEvents())
}

func (s *engineSuite) TestSignalWorkflowExecution() {
	signalRequest := &historyservice.SignalWorkflowExecutionRequest{}
	err := s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.EqualError(err, "Missing namespace UUID.")

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	signalName := "my signal name"
	input := payload.EncodeString("test input")
	signalRequest = &historyservice.SignalWorkflowExecutionRequest{
		NamespaceId: testNamespaceID,
		SignalRequest: &workflowservice.SignalWorkflowExecutionRequest{
			Namespace:         testNamespaceID,
			WorkflowExecution: &we,
			Identity:          identity,
			SignalName:        signalName,
			Input:             input,
		},
	}

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	ms.ExecutionInfo.NamespaceID = testNamespaceID
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(&persistence.AppendHistoryNodesResponse{Size: 0}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.Nil(err)
}

// Test signal decision by adding request ID
func (s *engineSuite) TestSignalWorkflowExecution_DuplicateRequest() {
	signalRequest := &historyservice.SignalWorkflowExecutionRequest{}
	err := s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.EqualError(err, "Missing namespace UUID.")

	we := executionpb.WorkflowExecution{
		WorkflowId: "wId2",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	signalName := "my signal name 2"
	input := payload.EncodeString("test input 2")
	requestID := uuid.New()
	signalRequest = &historyservice.SignalWorkflowExecutionRequest{
		NamespaceId: testNamespaceID,
		SignalRequest: &workflowservice.SignalWorkflowExecutionRequest{
			Namespace:         testNamespaceID,
			WorkflowExecution: &we,
			Identity:          identity,
			SignalName:        signalName,
			Input:             input,
			RequestId:         requestID,
		},
	}

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, we, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	// assume duplicate request id
	ms.SignalRequestedIDs = make(map[string]struct{})
	ms.SignalRequestedIDs[requestID] = struct{}{}
	ms.ExecutionInfo.NamespaceID = testNamespaceID
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.Nil(err)
}

func (s *engineSuite) TestSignalWorkflowExecution_Failed() {
	signalRequest := &historyservice.SignalWorkflowExecutionRequest{}
	err := s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.EqualError(err, "Missing namespace UUID.")

	we := &executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	signalName := "my signal name"
	input := payload.EncodeString("test input")
	signalRequest = &historyservice.SignalWorkflowExecutionRequest{
		NamespaceId: testNamespaceID,
		SignalRequest: &workflowservice.SignalWorkflowExecutionRequest{
			Namespace:         testNamespaceID,
			WorkflowExecution: we,
			Identity:          identity,
			SignalName:        signalName,
			Input:             input,
		},
	}

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), we.GetRunId())
	addWorkflowExecutionStartedEvent(msBuilder, *we, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	ms.ExecutionInfo.State = persistence.WorkflowStateCompleted
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()

	err = s.mockHistoryEngine.SignalWorkflowExecution(context.Background(), signalRequest)
	s.EqualError(err, "workflow execution already completed")
}

func (s *engineSuite) TestRemoveSignalMutableState() {
	removeRequest := &historyservice.RemoveSignalMutableStateRequest{}
	err := s.mockHistoryEngine.RemoveSignalMutableState(context.Background(), removeRequest)
	s.EqualError(err, "Missing namespace UUID.")

	execution := executionpb.WorkflowExecution{
		WorkflowId: "wId",
		RunId:      testRunID,
	}
	tasklist := "testTaskList"
	identity := "testIdentity"
	requestID := uuid.New()
	removeRequest = &historyservice.RemoveSignalMutableStateRequest{
		NamespaceId:       testNamespaceID,
		WorkflowExecution: &execution,
		RequestId:         requestID,
	}

	msBuilder := newMutableStateBuilderWithEventV2(s.mockHistoryEngine.shard, s.eventsCache,
		loggerimpl.NewDevelopmentForTest(s.Suite), testRunID)
	addWorkflowExecutionStartedEvent(msBuilder, execution, "wType", tasklist, payload.EncodeString("input"), 100, 200, identity)
	addDecisionTaskScheduledEvent(msBuilder)
	ms := createMutableState(msBuilder)
	ms.ExecutionInfo.NamespaceID = testNamespaceID
	gwmsResponse := &persistence.GetWorkflowExecutionResponse{State: ms}

	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(gwmsResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	err = s.mockHistoryEngine.RemoveSignalMutableState(context.Background(), removeRequest)
	s.Nil(err)
}

func (s *engineSuite) getBuilder(testNamespaceID string, we executionpb.WorkflowExecution) mutableState {
	context, release, err := s.mockHistoryEngine.historyCache.getOrCreateWorkflowExecutionForBackground(testNamespaceID, we)
	if err != nil {
		return nil
	}
	defer release(nil)

	return context.(*workflowExecutionContextImpl).mutableState
}

func (s *engineSuite) getActivityScheduledEvent(msBuilder mutableState,
	scheduleID int64) *eventpb.HistoryEvent {
	event, _ := msBuilder.GetActivityScheduledEvent(scheduleID)
	return event
}

func (s *engineSuite) printHistory(builder mutableState) string {
	return builder.GetHistoryBuilder().GetHistory().String()
}

func addWorkflowExecutionStartedEventWithParent(builder mutableState, workflowExecution executionpb.WorkflowExecution,
	workflowType, taskList string, input *commonpb.Payload, executionStartToCloseTimeout, taskStartToCloseTimeout int32,
	parentInfo *executiongenpb.ParentExecutionInfo, identity string) *eventpb.HistoryEvent {

	startRequest := &workflowservice.StartWorkflowExecutionRequest{
		WorkflowId:                          workflowExecution.WorkflowId,
		WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
		TaskList:                            &tasklistpb.TaskList{Name: taskList},
		Input:                               input,
		ExecutionStartToCloseTimeoutSeconds: executionStartToCloseTimeout,
		TaskStartToCloseTimeoutSeconds:      taskStartToCloseTimeout,
		Identity:                            identity,
	}

	event, _ := builder.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId:         testNamespaceID,
			StartRequest:        startRequest,
			ParentExecutionInfo: parentInfo,
		},
	)

	return event
}

func addWorkflowExecutionStartedEvent(builder mutableState, workflowExecution executionpb.WorkflowExecution,
	workflowType, taskList string, input *commonpb.Payload, executionStartToCloseTimeout, taskStartToCloseTimeout int32,
	identity string) *eventpb.HistoryEvent {
	return addWorkflowExecutionStartedEventWithParent(builder, workflowExecution, workflowType, taskList, input,
		executionStartToCloseTimeout, taskStartToCloseTimeout, nil, identity)
}

func addDecisionTaskScheduledEvent(builder mutableState) *decisionInfo {
	di, _ := builder.AddDecisionTaskScheduledEvent(false)
	return di
}

func addDecisionTaskStartedEvent(builder mutableState, scheduleID int64, taskList,
	identity string) *eventpb.HistoryEvent {
	return addDecisionTaskStartedEventWithRequestID(builder, scheduleID, testRunID, taskList, identity)
}

func addDecisionTaskStartedEventWithRequestID(builder mutableState, scheduleID int64, requestID string,
	taskList, identity string) *eventpb.HistoryEvent {
	event, _, _ := builder.AddDecisionTaskStartedEvent(scheduleID, requestID, &workflowservice.PollForDecisionTaskRequest{
		TaskList: &tasklistpb.TaskList{Name: taskList},
		Identity: identity,
	})

	return event
}

func addDecisionTaskCompletedEvent(builder mutableState, scheduleID, startedID int64, identity string) *eventpb.HistoryEvent {
	event, _ := builder.AddDecisionTaskCompletedEvent(scheduleID, startedID, &workflowservice.RespondDecisionTaskCompletedRequest{
		Identity: identity,
	}, defaultHistoryMaxAutoResetPoints)

	builder.FlushBufferedEvents() // nolint:errcheck

	return event
}

func addActivityTaskScheduledEvent(
	builder mutableState,
	decisionCompletedID int64,
	activityID, activityType,
	taskList string,
	input *commonpb.Payload,
	scheduleToCloseTimeout int32,
	scheduleToStartTimeout int32,
	startToCloseTimeout int32,
	heartbeatTimeout int32,
) (*eventpb.HistoryEvent,
	*persistence.ActivityInfo) {

	event, ai, _ := builder.AddActivityTaskScheduledEvent(decisionCompletedID, &decisionpb.ScheduleActivityTaskDecisionAttributes{
		ActivityId:                    activityID,
		ActivityType:                  &commonpb.ActivityType{Name: activityType},
		TaskList:                      &tasklistpb.TaskList{Name: taskList},
		Input:                         input,
		ScheduleToCloseTimeoutSeconds: scheduleToCloseTimeout,
		ScheduleToStartTimeoutSeconds: scheduleToStartTimeout,
		StartToCloseTimeoutSeconds:    startToCloseTimeout,
		HeartbeatTimeoutSeconds:       heartbeatTimeout,
	})

	return event, ai
}

func addActivityTaskScheduledEventWithRetry(
	builder mutableState,
	decisionCompletedID int64,
	activityID, activityType,
	taskList string,
	input *commonpb.Payload,
	scheduleToCloseTimeout int32,
	scheduleToStartTimeout int32,
	startToCloseTimeout int32,
	heartbeatTimeout int32,
	retryPolicy *commonpb.RetryPolicy,
) (*eventpb.HistoryEvent, *persistence.ActivityInfo) {

	event, ai, _ := builder.AddActivityTaskScheduledEvent(decisionCompletedID, &decisionpb.ScheduleActivityTaskDecisionAttributes{
		ActivityId:                    activityID,
		ActivityType:                  &commonpb.ActivityType{Name: activityType},
		TaskList:                      &tasklistpb.TaskList{Name: taskList},
		Input:                         input,
		ScheduleToCloseTimeoutSeconds: scheduleToCloseTimeout,
		ScheduleToStartTimeoutSeconds: scheduleToStartTimeout,
		StartToCloseTimeoutSeconds:    startToCloseTimeout,
		HeartbeatTimeoutSeconds:       heartbeatTimeout,
		RetryPolicy:                   retryPolicy,
	})

	return event, ai
}

func addActivityTaskStartedEvent(builder mutableState, scheduleID int64, identity string) *eventpb.HistoryEvent {
	ai, _ := builder.GetActivityInfo(scheduleID)
	event, _ := builder.AddActivityTaskStartedEvent(ai, scheduleID, testRunID, identity)
	return event
}

func addActivityTaskCompletedEvent(builder mutableState, scheduleID, startedID int64, result *commonpb.Payload,
	identity string) *eventpb.HistoryEvent {
	event, _ := builder.AddActivityTaskCompletedEvent(scheduleID, startedID, &workflowservice.RespondActivityTaskCompletedRequest{
		Result:   result,
		Identity: identity,
	})

	return event
}

func addActivityTaskFailedEvent(builder mutableState, scheduleID, startedID int64, reason string, details *commonpb.Payload,
	identity string) *eventpb.HistoryEvent {
	event, _ := builder.AddActivityTaskFailedEvent(scheduleID, startedID, &workflowservice.RespondActivityTaskFailedRequest{
		Reason:   reason,
		Details:  details,
		Identity: identity,
	})

	return event
}

func addTimerStartedEvent(builder mutableState, decisionCompletedEventID int64, timerID string,
	timeOut int64) (*eventpb.HistoryEvent, *persistenceblobs.TimerInfo) {
	event, ti, _ := builder.AddTimerStartedEvent(decisionCompletedEventID,
		&decisionpb.StartTimerDecisionAttributes{
			TimerId:                   timerID,
			StartToFireTimeoutSeconds: timeOut,
		})
	return event, ti
}

func addTimerFiredEvent(mutableState mutableState, timerID string) *eventpb.HistoryEvent {
	event, _ := mutableState.AddTimerFiredEvent(timerID)
	return event
}

func addRequestCancelInitiatedEvent(builder mutableState, decisionCompletedEventID int64,
	cancelRequestID, namespace, workflowID, runID string) (*eventpb.HistoryEvent, *persistenceblobs.RequestCancelInfo) {
	event, rci, _ := builder.AddRequestCancelExternalWorkflowExecutionInitiatedEvent(decisionCompletedEventID,
		cancelRequestID, &decisionpb.RequestCancelExternalWorkflowExecutionDecisionAttributes{
			Namespace:  namespace,
			WorkflowId: workflowID,
			RunId:      runID,
		})

	return event, rci
}

func addCancelRequestedEvent(builder mutableState, initiatedID int64, namespace, workflowID, runID string) *eventpb.HistoryEvent {
	event, _ := builder.AddExternalWorkflowExecutionCancelRequested(initiatedID, namespace, workflowID, runID)
	return event
}

func addRequestSignalInitiatedEvent(builder mutableState, decisionCompletedEventID int64,
	signalRequestID, namespace, workflowID, runID, signalName string, input *commonpb.Payload, control string) (*eventpb.HistoryEvent, *persistenceblobs.SignalInfo) {
	event, si, _ := builder.AddSignalExternalWorkflowExecutionInitiatedEvent(decisionCompletedEventID, signalRequestID,
		&decisionpb.SignalExternalWorkflowExecutionDecisionAttributes{
			Namespace: namespace,
			Execution: &executionpb.WorkflowExecution{
				WorkflowId: workflowID,
				RunId:      runID,
			},
			SignalName: signalName,
			Input:      input,
			Control:    control,
		})

	return event, si
}

func addSignaledEvent(builder mutableState, initiatedID int64, namespace, workflowID, runID string, control string) *eventpb.HistoryEvent {
	event, _ := builder.AddExternalWorkflowExecutionSignaled(initiatedID, namespace, workflowID, runID, control)
	return event
}

func addStartChildWorkflowExecutionInitiatedEvent(builder mutableState, decisionCompletedID int64,
	createRequestID, namespace, workflowID, workflowType, tasklist string, input *commonpb.Payload,
	executionStartToCloseTimeout, taskStartToCloseTimeout int32) (*eventpb.HistoryEvent,
	*persistence.ChildExecutionInfo) {

	event, cei, _ := builder.AddStartChildWorkflowExecutionInitiatedEvent(decisionCompletedID, createRequestID,
		&decisionpb.StartChildWorkflowExecutionDecisionAttributes{
			Namespace:                           namespace,
			WorkflowId:                          workflowID,
			WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
			TaskList:                            &tasklistpb.TaskList{Name: tasklist},
			Input:                               input,
			ExecutionStartToCloseTimeoutSeconds: executionStartToCloseTimeout,
			TaskStartToCloseTimeoutSeconds:      taskStartToCloseTimeout,
			Control:                             "",
		})
	return event, cei
}

func addChildWorkflowExecutionStartedEvent(builder mutableState, initiatedID int64, namespace, workflowID, runID string,
	workflowType string) *eventpb.HistoryEvent {
	event, _ := builder.AddChildWorkflowExecutionStartedEvent(
		namespace,
		&executionpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		&commonpb.WorkflowType{Name: workflowType},
		initiatedID,
		&commonpb.Header{},
	)
	return event
}

func addChildWorkflowExecutionCompletedEvent(builder mutableState, initiatedID int64, childExecution *executionpb.WorkflowExecution,
	attributes *eventpb.WorkflowExecutionCompletedEventAttributes) *eventpb.HistoryEvent {
	event, _ := builder.AddChildWorkflowExecutionCompletedEvent(initiatedID, childExecution, attributes)
	return event
}

func addCompleteWorkflowEvent(builder mutableState, decisionCompletedEventID int64,
	result *commonpb.Payload) *eventpb.HistoryEvent {
	event, _ := builder.AddCompletedWorkflowEvent(decisionCompletedEventID, &decisionpb.CompleteWorkflowExecutionDecisionAttributes{
		Result: result,
	})
	return event
}

func addFailWorkflowEvent(
	builder mutableState,
	decisionCompletedEventID int64,
	reason string,
	details *commonpb.Payload,
) *eventpb.HistoryEvent {
	event, _ := builder.AddFailWorkflowEvent(decisionCompletedEventID, &decisionpb.FailWorkflowExecutionDecisionAttributes{
		Reason:  reason,
		Details: details,
	})
	return event
}

func newMutableStateBuilderWithEventV2(shard ShardContext, eventsCache eventsCache,
	logger log.Logger, runID string) *mutableStateBuilder {

	msBuilder := newMutableStateBuilder(shard, eventsCache, logger, testLocalNamespaceEntry)
	_ = msBuilder.SetHistoryTree(primitives.MustParseUUID(runID))

	return msBuilder
}

func newMutableStateBuilderWithReplicationStateWithEventV2(shard ShardContext, eventsCache eventsCache,
	logger log.Logger, version int64, runID string) *mutableStateBuilder {

	msBuilder := newMutableStateBuilderWithReplicationState(shard, eventsCache, logger, testGlobalNamespaceEntry)
	msBuilder.GetReplicationState().StartVersion = version
	err := msBuilder.UpdateCurrentVersion(version, true)
	if err != nil {
		logger.Error("update current version error", tag.Error(err))
	}
	_ = msBuilder.SetHistoryTree(primitives.MustParseUUID(runID))

	return msBuilder
}

func createMutableState(ms mutableState) *persistence.WorkflowMutableState {
	builder := ms.(*mutableStateBuilder)
	builder.FlushBufferedEvents() // nolint:errcheck
	info := copyWorkflowExecutionInfo(builder.executionInfo)
	stats := &persistence.ExecutionStats{}
	activityInfos := make(map[int64]*persistence.ActivityInfo)
	for id, info := range builder.pendingActivityInfoIDs {
		activityInfos[id] = copyActivityInfo(info)
	}
	timerInfos := make(map[string]*persistenceblobs.TimerInfo)
	for id, info := range builder.pendingTimerInfoIDs {
		timerInfos[id] = copyTimerInfo(info)
	}
	cancellationInfos := make(map[int64]*persistenceblobs.RequestCancelInfo)
	for id, info := range builder.pendingRequestCancelInfoIDs {
		cancellationInfos[id] = copyCancellationInfo(info)
	}
	signalInfos := make(map[int64]*persistenceblobs.SignalInfo)
	for id, info := range builder.pendingSignalInfoIDs {
		signalInfos[id] = copySignalInfo(info)
	}
	childInfos := make(map[int64]*persistence.ChildExecutionInfo)
	for id, info := range builder.pendingChildExecutionInfoIDs {
		childInfos[id] = copyChildInfo(info)
	}

	builder.FlushBufferedEvents() // nolint:errcheck
	var bufferedEvents []*eventpb.HistoryEvent
	if len(builder.bufferedEvents) > 0 {
		bufferedEvents = append(bufferedEvents, builder.bufferedEvents...)
	}
	if len(builder.updateBufferedEvents) > 0 {
		bufferedEvents = append(bufferedEvents, builder.updateBufferedEvents...)
	}
	var replicationState *persistence.ReplicationState
	if builder.replicationState != nil {
		replicationState = copyReplicationState(builder.replicationState)
	}

	return &persistence.WorkflowMutableState{
		ExecutionInfo:       info,
		ExecutionStats:      stats,
		ActivityInfos:       activityInfos,
		TimerInfos:          timerInfos,
		BufferedEvents:      bufferedEvents,
		SignalInfos:         signalInfos,
		RequestCancelInfos:  cancellationInfos,
		ChildExecutionInfos: childInfos,
		ReplicationState:    replicationState,
	}
}

func copyWorkflowExecutionInfo(sourceInfo *persistence.WorkflowExecutionInfo) *persistence.WorkflowExecutionInfo {
	return &persistence.WorkflowExecutionInfo{
		NamespaceID:                        sourceInfo.NamespaceID,
		WorkflowID:                         sourceInfo.WorkflowID,
		RunID:                              sourceInfo.RunID,
		ParentNamespaceID:                  sourceInfo.ParentNamespaceID,
		ParentWorkflowID:                   sourceInfo.ParentWorkflowID,
		ParentRunID:                        sourceInfo.ParentRunID,
		InitiatedID:                        sourceInfo.InitiatedID,
		CompletionEventBatchID:             sourceInfo.CompletionEventBatchID,
		CompletionEvent:                    sourceInfo.CompletionEvent,
		TaskList:                           sourceInfo.TaskList,
		StickyTaskList:                     sourceInfo.StickyTaskList,
		StickyScheduleToStartTimeout:       sourceInfo.StickyScheduleToStartTimeout,
		WorkflowTypeName:                   sourceInfo.WorkflowTypeName,
		WorkflowTimeout:                    sourceInfo.WorkflowTimeout,
		DecisionStartToCloseTimeout:        sourceInfo.DecisionStartToCloseTimeout,
		State:                              sourceInfo.State,
		Status:                             sourceInfo.Status,
		LastFirstEventID:                   sourceInfo.LastFirstEventID,
		LastEventTaskID:                    sourceInfo.LastEventTaskID,
		NextEventID:                        sourceInfo.NextEventID,
		LastProcessedEvent:                 sourceInfo.LastProcessedEvent,
		StartTimestamp:                     sourceInfo.StartTimestamp,
		LastUpdatedTimestamp:               sourceInfo.LastUpdatedTimestamp,
		CreateRequestID:                    sourceInfo.CreateRequestID,
		SignalCount:                        sourceInfo.SignalCount,
		DecisionVersion:                    sourceInfo.DecisionVersion,
		DecisionScheduleID:                 sourceInfo.DecisionScheduleID,
		DecisionStartedID:                  sourceInfo.DecisionStartedID,
		DecisionRequestID:                  sourceInfo.DecisionRequestID,
		DecisionTimeout:                    sourceInfo.DecisionTimeout,
		DecisionAttempt:                    sourceInfo.DecisionAttempt,
		DecisionStartedTimestamp:           sourceInfo.DecisionStartedTimestamp,
		DecisionOriginalScheduledTimestamp: sourceInfo.DecisionOriginalScheduledTimestamp,
		CancelRequested:                    sourceInfo.CancelRequested,
		CancelRequestID:                    sourceInfo.CancelRequestID,
		CronSchedule:                       sourceInfo.CronSchedule,
		ClientLibraryVersion:               sourceInfo.ClientLibraryVersion,
		ClientFeatureVersion:               sourceInfo.ClientFeatureVersion,
		ClientImpl:                         sourceInfo.ClientImpl,
		AutoResetPoints:                    sourceInfo.AutoResetPoints,
		Memo:                               sourceInfo.Memo,
		SearchAttributes:                   sourceInfo.SearchAttributes,
		Attempt:                            sourceInfo.Attempt,
		HasRetryPolicy:                     sourceInfo.HasRetryPolicy,
		InitialInterval:                    sourceInfo.InitialInterval,
		BackoffCoefficient:                 sourceInfo.BackoffCoefficient,
		MaximumInterval:                    sourceInfo.MaximumInterval,
		ExpirationTime:                     sourceInfo.ExpirationTime,
		MaximumAttempts:                    sourceInfo.MaximumAttempts,
		NonRetriableErrors:                 sourceInfo.NonRetriableErrors,
		BranchToken:                        sourceInfo.BranchToken,
		ExpirationSeconds:                  sourceInfo.ExpirationSeconds,
	}
}

func copyHistoryEvent(source *eventpb.HistoryEvent) *eventpb.HistoryEvent {
	if source == nil {
		return nil
	}

	bytes, err := source.Marshal()
	if err != nil {
		panic(err)
	}

	result := &eventpb.HistoryEvent{}
	err = result.Unmarshal(bytes)
	if err != nil {
		panic(err)
	}
	return result
}

func copyActivityInfo(sourceInfo *persistence.ActivityInfo) *persistence.ActivityInfo {
	return &persistence.ActivityInfo{
		Version:                  sourceInfo.Version,
		ScheduleID:               sourceInfo.ScheduleID,
		ScheduledEventBatchID:    sourceInfo.ScheduledEventBatchID,
		ScheduledEvent:           copyHistoryEvent(sourceInfo.ScheduledEvent),
		StartedID:                sourceInfo.StartedID,
		StartedEvent:             copyHistoryEvent(sourceInfo.StartedEvent),
		ActivityID:               sourceInfo.ActivityID,
		RequestID:                sourceInfo.RequestID,
		Details:                  proto.Clone(sourceInfo.Details).(*commonpb.Payload),
		ScheduledTime:            sourceInfo.ScheduledTime,
		StartedTime:              sourceInfo.StartedTime,
		ScheduleToStartTimeout:   sourceInfo.ScheduleToStartTimeout,
		ScheduleToCloseTimeout:   sourceInfo.ScheduleToCloseTimeout,
		StartToCloseTimeout:      sourceInfo.StartToCloseTimeout,
		HeartbeatTimeout:         sourceInfo.HeartbeatTimeout,
		LastHeartBeatUpdatedTime: sourceInfo.LastHeartBeatUpdatedTime,
		CancelRequested:          sourceInfo.CancelRequested,
		CancelRequestID:          sourceInfo.CancelRequestID,
		TimerTaskStatus:          sourceInfo.TimerTaskStatus,
		Attempt:                  sourceInfo.Attempt,
		NamespaceID:              sourceInfo.NamespaceID,
		StartedIdentity:          sourceInfo.StartedIdentity,
		TaskList:                 sourceInfo.TaskList,
		HasRetryPolicy:           sourceInfo.HasRetryPolicy,
		InitialInterval:          sourceInfo.InitialInterval,
		BackoffCoefficient:       sourceInfo.BackoffCoefficient,
		MaximumInterval:          sourceInfo.MaximumInterval,
		ExpirationTime:           sourceInfo.ExpirationTime,
		MaximumAttempts:          sourceInfo.MaximumAttempts,
		NonRetriableErrors:       sourceInfo.NonRetriableErrors,
		LastFailureReason:        sourceInfo.LastFailureReason,
		LastWorkerIdentity:       sourceInfo.LastWorkerIdentity,
		LastFailureDetails:       sourceInfo.LastFailureDetails,
		// Not written to database - This is used only for deduping heartbeat timer creation
		LastHeartbeatTimeoutVisibilityInSeconds: sourceInfo.LastHeartbeatTimeoutVisibilityInSeconds,
	}
}

func copyTimerInfo(sourceInfo *persistenceblobs.TimerInfo) *persistenceblobs.TimerInfo {
	return &persistenceblobs.TimerInfo{
		Version:    sourceInfo.GetVersion(),
		TimerId:    sourceInfo.GetTimerId(),
		StartedId:  sourceInfo.GetStartedId(),
		ExpiryTime: sourceInfo.GetExpiryTime(),
		TaskStatus: sourceInfo.GetTaskStatus(),
	}
}

func copyCancellationInfo(sourceInfo *persistenceblobs.RequestCancelInfo) *persistenceblobs.RequestCancelInfo {
	return &persistenceblobs.RequestCancelInfo{
		Version:         sourceInfo.Version,
		InitiatedId:     sourceInfo.GetInitiatedId(),
		CancelRequestId: sourceInfo.GetCancelRequestId(),
	}
}

func copySignalInfo(sourceInfo *persistenceblobs.SignalInfo) *persistenceblobs.SignalInfo {
	result := &persistenceblobs.SignalInfo{
		Version:     sourceInfo.GetVersion(),
		InitiatedId: sourceInfo.GetInitiatedId(),
		RequestId:   sourceInfo.GetRequestId(),
		Name:        sourceInfo.GetName(),
	}
	result.Input = proto.Clone(sourceInfo.Input).(*commonpb.Payload)
	result.Control = sourceInfo.Control
	return result
}

func copyChildInfo(sourceInfo *persistence.ChildExecutionInfo) *persistence.ChildExecutionInfo {
	return &persistence.ChildExecutionInfo{
		Version:               sourceInfo.Version,
		InitiatedID:           sourceInfo.InitiatedID,
		InitiatedEventBatchID: sourceInfo.InitiatedEventBatchID,
		StartedID:             sourceInfo.StartedID,
		StartedWorkflowID:     sourceInfo.StartedWorkflowID,
		StartedRunID:          sourceInfo.StartedRunID,
		CreateRequestID:       sourceInfo.CreateRequestID,
		Namespace:             sourceInfo.Namespace,
		WorkflowTypeName:      sourceInfo.WorkflowTypeName,
		ParentClosePolicy:     sourceInfo.ParentClosePolicy,
		InitiatedEvent:        copyHistoryEvent(sourceInfo.InitiatedEvent),
		StartedEvent:          copyHistoryEvent(sourceInfo.StartedEvent),
	}
}

func copyReplicationState(source *persistence.ReplicationState) *persistence.ReplicationState {
	var lastReplicationInfo map[string]*replicationgenpb.ReplicationInfo
	if source.LastReplicationInfo != nil {
		lastReplicationInfo = map[string]*replicationgenpb.ReplicationInfo{}
		for k, v := range source.LastReplicationInfo {
			lastReplicationInfo[k] = &replicationgenpb.ReplicationInfo{
				Version:     v.Version,
				LastEventId: v.LastEventId,
			}
		}
	}

	return &persistence.ReplicationState{
		CurrentVersion:      source.CurrentVersion,
		StartVersion:        source.StartVersion,
		LastWriteVersion:    source.LastWriteVersion,
		LastWriteEventID:    source.LastWriteEventID,
		LastReplicationInfo: lastReplicationInfo,
	}
}
