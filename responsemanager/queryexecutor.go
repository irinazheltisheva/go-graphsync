package responsemanager

import (
	"context"
	"errors"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-graphsync"
	"github.com/ipfs/go-graphsync/cidset"
	"github.com/ipfs/go-graphsync/ipldutil"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/responsemanager/hooks"
	"github.com/ipfs/go-graphsync/responsemanager/peerresponsemanager"
	"github.com/ipfs/go-graphsync/responsemanager/runtraversal"
	ipld "github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
)

// TODO: Move this into a seperate module and fully seperate from the ResponseManager
type queryExecutor struct {
	requestHooks RequestHooks
	blockHooks   BlockHooks
	updateHooks  UpdateHooks
	peerManager  PeerManager
	loader       ipld.Loader
	queryQueue   QueryQueue
	messages     chan responseManagerMessage
	ctx          context.Context
	workSignal   chan struct{}
	ticker       *time.Ticker
}

func (qe *queryExecutor) processQueriesWorker() {
	const targetWork = 1
	taskDataChan := make(chan responseTaskData)
	var taskData responseTaskData
	for {
		pid, tasks, _ := qe.queryQueue.PopTasks(targetWork)
		for len(tasks) == 0 {
			select {
			case <-qe.ctx.Done():
				return
			case <-qe.workSignal:
				pid, tasks, _ = qe.queryQueue.PopTasks(targetWork)
			case <-qe.ticker.C:
				qe.queryQueue.ThawRound()
				pid, tasks, _ = qe.queryQueue.PopTasks(targetWork)
			}
		}
		for _, task := range tasks {
			key := task.Topic.(responseKey)
			select {
			case qe.messages <- &responseDataRequest{key, taskDataChan}:
			case <-qe.ctx.Done():
				return
			}
			select {
			case taskData = <-taskDataChan:
			case <-qe.ctx.Done():
				return
			}
			if taskData.empty {
				log.Info("Empty task on peer request stack")
				continue
			}
			status, err := qe.executeTask(key, taskData)
			select {
			case qe.messages <- &finishTaskRequest{key, status, err}:
			case <-qe.ctx.Done():
			}
		}
		qe.queryQueue.TasksDone(pid, tasks...)

	}

}

func (qe *queryExecutor) executeTask(key responseKey, taskData responseTaskData) (graphsync.ResponseStatusCode, error) {
	var err error
	loader := taskData.loader
	traverser := taskData.traverser
	if loader == nil || traverser == nil {
		loader, traverser, err = qe.prepareQuery(taskData.ctx, key.p, taskData.request)
		if err != nil {
			return graphsync.RequestFailedUnknown, err
		}
		select {
		case <-qe.ctx.Done():
			return graphsync.RequestFailedUnknown, errors.New("context cancelled")
		case qe.messages <- &setResponseDataRequest{key, loader, traverser}:
		}
	}
	return qe.executeQuery(key.p, taskData.request, loader, traverser, taskData.signals)
}

func (qe *queryExecutor) prepareQuery(ctx context.Context,
	p peer.ID,
	request gsmsg.GraphSyncRequest) (ipld.Loader, ipldutil.Traverser, error) {
	result := qe.requestHooks.ProcessRequestHooks(p, request)
	peerResponseSender := qe.peerManager.SenderForPeer(p)
	var validationErr error
	err := peerResponseSender.Transaction(request.ID(), func(transaction peerresponsemanager.PeerResponseTransactionSender) error {
		for _, extension := range result.Extensions {
			transaction.SendExtensionData(extension)
		}
		if result.Err != nil || !result.IsValidated {
			transaction.FinishWithError(graphsync.RequestFailedUnknown)
			validationErr = errors.New("request not valid")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if validationErr != nil {
		return nil, nil, validationErr
	}
	if err := qe.processDoNoSendCids(request, peerResponseSender); err != nil {
		return nil, nil, err
	}
	rootLink := cidlink.Link{Cid: request.Root()}
	traverser := ipldutil.TraversalBuilder{
		Root:     rootLink,
		Selector: request.Selector(),
		Chooser:  result.CustomChooser,
	}.Start(ctx)
	loader := result.CustomLoader
	if loader == nil {
		loader = qe.loader
	}
	return loader, traverser, nil
}

func (qe *queryExecutor) processDoNoSendCids(request gsmsg.GraphSyncRequest, peerResponseSender peerresponsemanager.PeerResponseSender) error {
	doNotSendCidsData, has := request.Extension(graphsync.ExtensionDoNotSendCIDs)
	if !has {
		return nil
	}
	cidSet, err := cidset.DecodeCidSet(doNotSendCidsData)
	if err != nil {
		peerResponseSender.FinishWithError(request.ID(), graphsync.RequestFailedUnknown)
		return err
	}
	links := make([]ipld.Link, 0, cidSet.Len())
	err = cidSet.ForEach(func(c cid.Cid) error {
		links = append(links, cidlink.Link{Cid: c})
		return nil
	})
	if err != nil {
		return err
	}
	peerResponseSender.IgnoreBlocks(request.ID(), links)
	return nil
}

func (qe *queryExecutor) executeQuery(
	p peer.ID,
	request gsmsg.GraphSyncRequest,
	loader ipld.Loader,
	traverser ipldutil.Traverser,
	signals signals) (graphsync.ResponseStatusCode, error) {
	updateChan := make(chan []gsmsg.GraphSyncRequest)
	peerResponseSender := qe.peerManager.SenderForPeer(p)
	err := runtraversal.RunTraversal(loader, traverser, func(link ipld.Link, data []byte) error {
		var err error
		_ = peerResponseSender.Transaction(request.ID(), func(transaction peerresponsemanager.PeerResponseTransactionSender) error {
			err = qe.checkForUpdates(p, request, signals, updateChan, transaction)
			if err != nil {
				if _, ok := err.(hooks.ErrPaused); ok {
					transaction.PauseRequest()
				}
				return nil
			}
			blockData := transaction.SendResponse(link, data)
			if blockData.BlockSize() > 0 {
				result := qe.blockHooks.ProcessBlockHooks(p, request, blockData)
				for _, extension := range result.Extensions {
					transaction.SendExtensionData(extension)
				}
				if _, ok := result.Err.(hooks.ErrPaused); ok {
					transaction.PauseRequest()
				}
				err = result.Err
			}
			return nil
		})
		return err
	})
	if err != nil {
		_, isPaused := err.(hooks.ErrPaused)
		if isPaused {
			return graphsync.RequestPaused, err
		}
		_, isCancelled := err.(ipldutil.ContextCancelError)
		if isCancelled {
			peerResponseSender.FinishWithCancel(request.ID())
			return graphsync.RequestFailedUnknown, err
		}
		peerResponseSender.FinishWithError(request.ID(), graphsync.RequestFailedUnknown)
		return graphsync.RequestFailedUnknown, err
	}
	return peerResponseSender.FinishRequest(request.ID()), nil
}

func (qe *queryExecutor) checkForUpdates(
	p peer.ID,
	request gsmsg.GraphSyncRequest,
	signals signals,
	updateChan chan []gsmsg.GraphSyncRequest,
	peerResponseSender peerresponsemanager.PeerResponseTransactionSender) error {
	for {
		select {
		case <-signals.stopSignal:
			return errors.New("response cancelled by responder")
		case <-signals.pauseSignal:
			return hooks.ErrPaused{}
		case <-signals.updateSignal:
			select {
			case qe.messages <- &responseUpdateRequest{responseKey{p, request.ID()}, updateChan}:
			case <-qe.ctx.Done():
			}
			select {
			case updates := <-updateChan:
				for _, update := range updates {
					result := qe.updateHooks.ProcessUpdateHooks(p, request, update)
					for _, extension := range result.Extensions {
						peerResponseSender.SendExtensionData(extension)
					}
					if result.Err != nil {
						return result.Err
					}
				}
			case <-qe.ctx.Done():
			}
		default:
			return nil
		}
	}
}