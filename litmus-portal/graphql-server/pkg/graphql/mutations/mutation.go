package mutations

import (
	"log"
	"strconv"
	"time"

	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/chaos-workflow/handler"

	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/graphql"

	"github.com/jinzhu/copier"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/graph/model"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/cluster"
	store "github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/data-store"
	database "github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/database/mongodb"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/pkg/graphql/subscriptions"
	"github.com/litmuschaos/litmus/litmus-portal/graphql-server/utils"
)

//ClusterRegister creates an entry for a new cluster in DB and generates the url used to apply manifest
func ClusterRegister(input model.ClusterInput) (*model.ClusterRegResponse, error) {
	clusterID := uuid.New().String()

	token, err := cluster.ClusterCreateJWT(clusterID)
	if err != nil {
		return &model.ClusterRegResponse{}, err
	}

	newCluster := database.Cluster{
		ClusterID:      clusterID,
		ClusterName:    input.ClusterName,
		Description:    input.Description,
		ProjectID:      input.ProjectID,
		AccessKey:      utils.RandomString(32),
		ClusterType:    input.ClusterType,
		PlatformName:   input.PlatformName,
		AgentNamespace: input.AgentNamespace,
		Serviceaccount: input.Serviceaccount,
		AgentScope:     input.AgentScope,
		AgentNsExists:  input.AgentNsExists,
		AgentSaExists:  input.AgentSaExists,
		CreatedAt:      strconv.FormatInt(time.Now().Unix(), 10),
		UpdatedAt:      strconv.FormatInt(time.Now().Unix(), 10),
		Token:          token,
		IsRemoved:      false,
	}

	err = database.InsertCluster(newCluster)
	if err != nil {
		return &model.ClusterRegResponse{}, err
	}

	log.Print("NEW CLUSTER REGISTERED : ID-", clusterID, " PID-", input.ProjectID)

	return &model.ClusterRegResponse{
		ClusterID:   newCluster.ClusterID,
		Token:       token,
		ClusterName: newCluster.ClusterName,
	}, nil
}

//ConfirmClusterRegistration takes the cluster_id and access_key from the subscriber and validates it, if validated generates and sends new access_key
func ConfirmClusterRegistration(identity model.ClusterIdentity, r store.StateData) (*model.ClusterConfirmResponse, error) {
	cluster, err := database.GetCluster(identity.ClusterID)
	if err != nil {
		return &model.ClusterConfirmResponse{IsClusterConfirmed: false}, err
	}

	if cluster.AccessKey == identity.AccessKey {
		newKey := utils.RandomString(32)
		time := strconv.FormatInt(time.Now().Unix(), 10)
		query := bson.D{{"cluster_id", identity.ClusterID}}
		update := bson.D{{"$unset", bson.D{{"token", ""}}}, {"$set", bson.D{{"access_key", newKey}, {"is_registered", true}, {"is_cluster_confirmed", true}, {"updated_at", time}}}}

		err = database.UpdateCluster(query, update)
		if err != nil {
			return &model.ClusterConfirmResponse{IsClusterConfirmed: false}, err
		}

		cluster.IsRegistered = true
		cluster.AccessKey = ""

		newCluster := model.Cluster{}
		copier.Copy(&newCluster, &cluster)

		log.Print("CLUSTER Confirmed : ID-", cluster.ClusterID, " PID-", cluster.ProjectID)
		subscriptions.SendClusterEvent("cluster-registration", "New Cluster", "New Cluster registration", newCluster, r)

		return &model.ClusterConfirmResponse{IsClusterConfirmed: true, NewClusterKey: &newKey, ClusterID: &cluster.ClusterID}, err
	}
	return &model.ClusterConfirmResponse{IsClusterConfirmed: false}, err
}

//NewEvent takes a event from a subscriber, validates identity and broadcasts the event to the users
func NewEvent(clusterEvent model.ClusterEventInput, r store.StateData) (string, error) {
	cluster, err := database.GetCluster(clusterEvent.ClusterID)
	if err != nil {
		return "", err
	}

	if cluster.AccessKey == clusterEvent.AccessKey && cluster.IsRegistered {
		log.Print("CLUSTER EVENT : ID-", cluster.ClusterID, " PID-", cluster.ProjectID)

		newCluster := model.Cluster{}
		copier.Copy(&newCluster, &cluster)

		subscriptions.SendClusterEvent("cluster-event", clusterEvent.EventName, clusterEvent.Description, newCluster, r)
		return "Event Published", nil
	}

	return "", errors.New("ERROR WITH CLUSTER EVENT")
}

// WorkFlowRunHandler Updates or Inserts a new Workflow Run into the DB
func WorkFlowRunHandler(input model.WorkflowRunInput, r store.StateData) (string, error) {
	cluster, err := cluster.VerifyCluster(*input.ClusterID)
	if err != nil {
		log.Print("ERROR", err)
		return "", err
	}

	//err = database.UpdateWorkflowRun(database.WorkflowRun(newWorkflowRun))
	count, err := database.UpdateWorkflowRun(input.WorkflowID, database.WorkflowRun{
		WorkflowRunID: input.WorkflowRunID,
		LastUpdated:   strconv.FormatInt(time.Now().Unix(), 10),
		ExecutionData: input.ExecutionData,
		Completed:     input.Completed,
	})
	if err != nil {
		log.Print("ERROR", err)
		return "", err
	}

	if count == 0 {
		return "Workflow Run Discarded[Duplicate Event]", nil
	}

	handler.SendWorkflowEvent(model.WorkflowRun{
		ClusterID:     cluster.ClusterID,
		ClusterName:   cluster.ClusterName,
		ProjectID:     cluster.ProjectID,
		LastUpdated:   strconv.FormatInt(time.Now().Unix(), 10),
		WorkflowRunID: input.WorkflowRunID,
		WorkflowName:  input.WorkflowName,
		ExecutionData: input.ExecutionData,
		WorkflowID:    input.WorkflowID,
	}, &r)

	return "Workflow Run Accepted", nil
}

// LogsHandler receives logs from the workflow-agent and publishes to frontend clients
func LogsHandler(podLog model.PodLog, r store.StateData) (string, error) {
	_, err := cluster.VerifyCluster(*podLog.ClusterID)
	if err != nil {
		log.Print("ERROR", err)
		return "", err
	}
	if reqChan, ok := r.WorkflowLog[podLog.RequestID]; ok {
		resp := model.PodLogResponse{
			PodName:       podLog.PodName,
			WorkflowRunID: podLog.WorkflowRunID,
			PodType:       podLog.PodType,
			Log:           podLog.Log,
		}
		reqChan <- &resp
		close(reqChan)
		return "LOGS SENT SUCCESSFULLY", nil
	}
	return "LOG REQUEST CANCELLED", nil
}

func DeleteCluster(cluster_id string, r store.StateData) (string, error) {
	time := strconv.FormatInt(time.Now().Unix(), 10)

	query := bson.D{{"cluster_id", cluster_id}}
	update := bson.D{{"$set", bson.D{{"is_removed", true}, {"updated_at", time}}}}

	err := database.UpdateCluster(query, update)
	if err != nil {
		return "", err
	}
	cluster, err := database.GetCluster(cluster_id)
	if err != nil {
		return "", nil
	}

	requests := []string{
		`{
			"apiVersion": "apps/v1",
			"kind": "Deployment",
			"metadata": {
				"name": "subscriber",
				"namespace": ` + *cluster.AgentNamespace + ` 
			}
		}`,
		`{
		   "apiVersion": "v1",
		   "kind": "ConfigMap",
		   "metadata": {
			  "name": "litmus-portal-config",
			  "namespace": ` + *cluster.AgentNamespace + ` 
		   }
		}`,
	}

	for _, request := range requests {
		subscriptions.SendRequestToSubscriber(graphql.SubscriberRequests{
			K8sManifest: request,
			RequestType: "delete",
			ProjectID:   cluster.ProjectID,
			ClusterID:   cluster_id,
			Namespace:   *cluster.AgentNamespace,
		}, r)
	}

	return "Successfully deleted cluster", nil
}
