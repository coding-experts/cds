package workflow

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/go-gorp/gorp"
	"github.com/lib/pq"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/cache"
	"github.com/ovh/cds/engine/api/environment"
	"github.com/ovh/cds/engine/api/secret"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

// UpdateNodeJobRunStatus Update status of an workflow_node_run_job
func UpdateNodeJobRunStatus(db gorp.SqlExecutor, store cache.Store, p *sdk.Project, job *sdk.WorkflowNodeJobRun, status sdk.Status, chanEvent chan<- interface{}) error {
	log.Debug("UpdateNodeJobRunStatus> job.ID=%d status=%s", job.ID, status.String())

	node, errLoad := LoadNodeRunByID(db, job.WorkflowNodeRunID)
	if errLoad != nil {
		sdk.WrapError(errLoad, "workflow.UpdateNodeJobRunStatus> Unable to load node run id %d", job.WorkflowNodeRunID)
	}

	var query string
	query = `SELECT status FROM workflow_node_run_job WHERE id = $1`
	var currentStatus string
	if err := db.QueryRow(query, job.ID).Scan(&currentStatus); err != nil {
		return sdk.WrapError(err, "workflow.UpdateNodeJobRunStatus> Cannot lock node job run %d", job.ID)
	}

	switch status {
	case sdk.StatusBuilding:
		if currentStatus != sdk.StatusWaiting.String() {
			return fmt.Errorf("workflow.UpdateNodeJobRunStatus> Cannot update status of WorkflowNodeJobRun %d to %s, expected current status %s, got %s",
				job.ID, status, sdk.StatusWaiting, currentStatus)
		}
		job.Start = time.Now()
		job.Status = status.String()

	case sdk.StatusFail, sdk.StatusSuccess, sdk.StatusDisabled, sdk.StatusSkipped, sdk.StatusStopped:
		if currentStatus != string(sdk.StatusWaiting) && currentStatus != string(sdk.StatusBuilding) && status != sdk.StatusDisabled && status != sdk.StatusSkipped {
			log.Debug("workflow.UpdateNodeJobRunStatus> Status is %s, cannot update %d to %s", currentStatus, job.ID, status)
			// too late, Nate
			return nil
		}
		job.Done = time.Now()
		job.Status = status.String()

		wf, errLoadWf := LoadRunByID(db, node.WorkflowRunID)
		if errLoadWf != nil {
			return sdk.WrapError(errLoadWf, "workflow.UpdateNodeJobRunStatus> Unable to load run id %d", node.WorkflowRunID)
		}

		wf.LastExecution = time.Now()
		if err := updateWorkflowRun(db, wf); err != nil {
			return sdk.WrapError(err, "workflow.UpdateNodeJobRunStatus> Cannot update WorkflowRun %d", wf.ID)
		}
	default:
		return fmt.Errorf("workflow.UpdateNodeJobRunStatus> Cannot update WorkflowNodeJobRun %d to status %v", job.ID, status.String())
	}

	//If the job has been set to building, set the stage to building
	var stageIndex int
	for i := range node.Stages {
		s := &node.Stages[i]
		for _, j := range s.Jobs {
			if j.Action.ID == job.Job.Job.Action.ID {
				stageIndex = i
			}
		}
	}

	if err := UpdateNodeJobRun(db, store, p, job); err != nil {
		return sdk.WrapError(err, "workflow.UpdateNodeJobRunStatus> Cannot update WorkflowNodeJobRun %d", job.ID)
	}
	// Push update on node run job
	if chanEvent != nil {
		chanEvent <- *job
	}

	if status == sdk.StatusBuilding {
		// Sync job status in noderun
		nodeRun, errNR := LoadNodeRunByID(db, node.ID)
		if errNR != nil {
			return sdk.WrapError(errNR, "workflow.UpdateNodeJobRunStatus> Cannot Load and lock node run %d", node.ID)
		}
		return syncTakeJobInNodeRun(db, nodeRun, job, stageIndex, chanEvent)
	}
	return execute(db, store, p, node, chanEvent)
}

// AddSpawnInfosNodeJobRun saves spawn info before starting worker
func AddSpawnInfosNodeJobRun(db gorp.SqlExecutor, store cache.Store, p *sdk.Project, id int64, infos []sdk.SpawnInfo) (*sdk.WorkflowNodeJobRun, error) {
	j, err := LoadAndLockNodeJobRunWait(db, store, id)
	if err != nil {
		return nil, sdk.WrapError(err, "AddSpawnInfosNodeJobRun> Cannot load node job run")
	}
	if err := prepareSpawnInfos(j, infos); err != nil {
		return nil, sdk.WrapError(err, "AddSpawnInfosNodeJobRun> Cannot prepare spawn infos")
	}

	if err := UpdateNodeJobRun(db, store, p, j); err != nil {
		return nil, sdk.WrapError(err, "AddSpawnInfosNodeJobRun> Cannot update node job run")
	}
	return j, nil
}

func prepareSpawnInfos(j *sdk.WorkflowNodeJobRun, infos []sdk.SpawnInfo) error {
	now := time.Now()
	for _, info := range infos {
		j.SpawnInfos = append(j.SpawnInfos, sdk.SpawnInfo{
			APITime:    now,
			RemoteTime: info.RemoteTime,
			Message:    info.Message,
		})
	}
	return nil
}

// TakeNodeJobRun Take an a job run for update
func TakeNodeJobRun(db gorp.SqlExecutor, store cache.Store, p *sdk.Project, id int64, workerModel string, workerName string, workerID string, infos []sdk.SpawnInfo, chanEvent chan<- interface{}) (*sdk.WorkflowNodeJobRun, error) {
	job, err := LoadAndLockNodeJobRunWait(db, store, id)
	if err != nil {
		if errPG, ok := err.(*pq.Error); ok && errPG.Code == "55P03" {
			err = sdk.ErrJobAlreadyBooked
		}
		return nil, sdk.WrapError(err, "TakeNodeJobRun> Cannot load node job run")
	}
	if job.Status != sdk.StatusWaiting.String() {
		k := keyBookJob(id)
		h := sdk.Hatchery{}
		if store.Get(k, &h) {
			return nil, sdk.WrapError(sdk.ErrAlreadyTaken, "TakeNodeJobRun> job %d is not waiting status and was booked by hatchery %d. Current status:%s", id, h.ID, job.Status)
		}
		return nil, sdk.WrapError(sdk.ErrAlreadyTaken, "TakeNodeJobRun> job %d is not waiting status. Current status:%s", id, job.Status)
	}

	job.Model = workerModel
	job.Job.WorkerName = workerName
	job.Job.WorkerID = workerID
	job.Start = time.Now()

	if err := prepareSpawnInfos(job, infos); err != nil {
		return nil, sdk.WrapError(err, "TakeNodeJobRun> Cannot prepare spawn infos")
	}

	if err := UpdateNodeJobRunStatus(db, store, p, job, sdk.StatusBuilding, chanEvent); err != nil {
		log.Debug("TakeNodeJobRun> call UpdateNodeJobRunStatus on job %d set status from %s to %s", job.ID, job.Status, sdk.StatusBuilding)
		return nil, sdk.WrapError(err, "TakeNodeJobRun>Cannot update node job run")
	}

	return job, nil
}

// LoadNodeJobRunKeys loads all keys for a job run
func LoadNodeJobRunKeys(db gorp.SqlExecutor, store cache.Store, job *sdk.WorkflowNodeJobRun, nodeRun *sdk.WorkflowNodeRun, w *sdk.WorkflowRun, p *sdk.Project) ([]sdk.Parameter, []sdk.Variable, error) {
	params := []sdk.Parameter{}
	secrets := []sdk.Variable{}

	for _, k := range p.Keys {
		params = append(params, sdk.Parameter{
			Name:  "cds.proj." + k.Name + ".pub",
			Type:  "string",
			Value: k.Public,
		})
		params = append(params, sdk.Parameter{
			Name:  "cds.proj." + k.Name + ".id",
			Type:  "string",
			Value: k.KeyID,
		})
		secrets = append(secrets, sdk.Variable{
			Name:  "cds.proj." + k.Name + ".priv",
			Type:  "string",
			Value: k.Private,
		})
	}

	//Load node definition
	n := w.Workflow.GetNode(nodeRun.WorkflowNodeID)
	if n == nil {
		return nil, nil, sdk.WrapError(fmt.Errorf("Unable to find node %d in workflow", nodeRun.WorkflowNodeID), "LoadNodeJobRunSecrets>")
	}
	if n.Context != nil && n.Context.Application != nil {
		a, errA := application.LoadByID(db, store, n.Context.Application.ID, nil, application.LoadOptions.WithKeys)
		if errA != nil {
			return nil, nil, sdk.WrapError(errA, "loadActionBuildKeys> Cannot load application keys")
		}
		for _, k := range a.Keys {
			params = append(params, sdk.Parameter{
				Name:  "cds.app." + k.Name + ".pub",
				Type:  "string",
				Value: k.Public,
			})
			params = append(params, sdk.Parameter{
				Name:  "cds.app." + k.Name + ".id",
				Type:  "string",
				Value: k.KeyID,
			})
			secrets = append(secrets, sdk.Variable{
				Name:  "cds.app." + k.Name + ".priv",
				Type:  "string",
				Value: k.Private,
			})
		}
	}

	if n.Context != nil && n.Context.Environment != nil && n.Context.Environment.ID != sdk.DefaultEnv.ID {
		e, errE := environment.LoadEnvironmentByID(db, n.Context.Environment.ID)
		if errE != nil {
			return nil, nil, sdk.WrapError(errE, "loadActionBuildKeys> Cannot load environment keys")
		}
		for _, k := range e.Keys {
			params = append(params, sdk.Parameter{
				Name:  "cds.env." + k.Name + ".pub",
				Type:  "string",
				Value: k.Public,
			})
			params = append(params, sdk.Parameter{
				Name:  "cds.env." + k.Name + ".id",
				Type:  "string",
				Value: k.KeyID,
			})
			secrets = append(secrets, sdk.Variable{
				Name:  "cds.env." + k.Name + ".priv",
				Type:  "string",
				Value: k.Private,
			})
		}

	}
	return params, secrets, nil
}

// LoadNodeJobRunSecrets loads all secrets for a job run
func LoadNodeJobRunSecrets(db gorp.SqlExecutor, store cache.Store, job *sdk.WorkflowNodeJobRun, nodeRun *sdk.WorkflowNodeRun, w *sdk.WorkflowRun, pv []sdk.Variable) ([]sdk.Variable, error) {
	var secrets []sdk.Variable

	pv = sdk.VariablesFilter(pv, sdk.SecretVariable, sdk.KeyVariable)
	pv = sdk.VariablesPrefix(pv, "cds.proj.")
	secrets = append(secrets, pv...)

	//Load node definition
	n := w.Workflow.GetNode(nodeRun.WorkflowNodeID)
	if n == nil {
		return nil, sdk.WrapError(fmt.Errorf("Unable to find node %d in workflow", nodeRun.WorkflowNodeID), "LoadNodeJobRunSecrets>")
	}

	//Application variables
	av := []sdk.Variable{}
	if n.Context != nil && n.Context.Application != nil {
		appv, errA := application.GetAllVariableByID(db, n.Context.Application.ID, application.WithClearPassword())
		if errA != nil {
			return nil, sdk.WrapError(errA, "LoadNodeJobRunSecrets> Cannot load application variables")
		}
		av = sdk.VariablesFilter(appv, sdk.SecretVariable, sdk.KeyVariable)
		av = sdk.VariablesPrefix(av, "cds.app.")
	}
	secrets = append(secrets, av...)

	//Environment variables
	ev := []sdk.Variable{}
	if n.Context != nil && n.Context.Environment != nil {
		envv, errE := environment.GetAllVariableByID(db, n.Context.Environment.ID, environment.WithClearPassword())
		if errE != nil {
			return nil, sdk.WrapError(errE, "LoadNodeJobRunSecrets> Cannot load environment variables")
		}
		ev = sdk.VariablesFilter(envv, sdk.SecretVariable, sdk.KeyVariable)
		ev = sdk.VariablesPrefix(ev, "cds.env.")
	}
	secrets = append(secrets, ev...)

	//Decrypt secrets
	for i := range secrets {
		s := &secrets[i]
		if err := secret.DecryptVariable(s); err != nil {
			return nil, sdk.WrapError(err, "LoadNodeJobRunSecrets> Unable to decrypt variables")
		}
	}
	return secrets, nil
}

//BookNodeJobRun  Book a job for a hatchery
func BookNodeJobRun(store cache.Store, id int64, hatchery *sdk.Hatchery) (*sdk.Hatchery, error) {
	k := keyBookJob(id)
	h := sdk.Hatchery{}
	if !store.Get(k, &h) {
		// job not already booked, book it for 2 min
		store.SetWithTTL(k, hatchery, 120)
		return nil, nil
	}
	return &h, sdk.WrapError(sdk.ErrJobAlreadyBooked, "BookNodeJobRun> job %d already booked by %s (%d)", id, h.Name, h.ID)
}

//AddLog adds a build log
func AddLog(db gorp.SqlExecutor, job *sdk.WorkflowNodeJobRun, logs *sdk.Log) error {
	if job != nil {
		logs.PipelineBuildJobID = job.ID
		logs.PipelineBuildID = job.WorkflowNodeRunID
	}

	existingLogs, errLog := LoadStepLogs(db, logs.PipelineBuildJobID, logs.StepOrder)
	if errLog != nil && errLog != sql.ErrNoRows {
		return sdk.WrapError(errLog, "AddLog> Cannot load existing logs")
	}

	if existingLogs == nil {
		if err := insertLog(db, logs); err != nil {
			return sdk.WrapError(err, "AddLog> Cannot insert log")
		}
	} else {
		existingLogs.Val += logs.Val
		existingLogs.LastModified = logs.LastModified
		existingLogs.Done = logs.Done
		if err := updateLog(db, existingLogs); err != nil {
			return sdk.WrapError(err, "AddLog> Cannot update log")
		}
	}
	return nil
}

// RestartWorkflowNodeJob restart all workflow node job and update logs to indicate restart
func RestartWorkflowNodeJob(db gorp.SqlExecutor, wNodeJob sdk.WorkflowNodeJobRun) error {
	for _, step := range wNodeJob.Job.StepStatus {
		if step.Status == sdk.StatusNeverBuilt.String() || step.Status == sdk.StatusSkipped.String() || step.Status == sdk.StatusDisabled.String() {
			continue
		}
		l, errL := LoadStepLogs(db, wNodeJob.ID, int64(step.StepOrder))
		if errL != nil {
			return sdk.WrapError(errL, "RestartWorkflowNodeJob> error while load step logs")
		}
		wNodeJob.Job.Reason = "Killed (Reason: Timeout)\n"

		if l != nil { // log could be nil here
			l.Val += "\n\n\n-=-=-=-=-=- Worker timeout: job replaced in queue -=-=-=-=-=-\n\n\n"
			if err := updateLog(db, l); err != nil {
				return sdk.WrapError(errL, "RestartWorkflowNodeJob> error while update step log")
			}
		}
	}
	if err := replaceWorkflowJobRunInQueue(db, wNodeJob); err != nil {
		return sdk.WrapError(err, "RestartWorkflowNodeJob> Cannot replace workflow job in queue")
	}

	return nil
}
