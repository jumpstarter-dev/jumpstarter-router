/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	pb "github.com/jumpstarter-dev/jumpstarter-protocol/go/jumpstarter/v1"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	jumpstarterdevv1alpha1 "github.com/jumpstarter-dev/jumpstarter-controller/api/v1alpha1"
)

// Reference: config/default/kustomization.yaml
const nameSpace = "jumpstarter-router-system"
const namePrefix = "jumpstarter-router-"

// ControlerService exposes a gRPC service
type ControllerService struct {
	pb.UnimplementedControllerServiceServer
	client.Client
	Scheme *runtime.Scheme
	listen sync.Map
}

type listenContext struct {
	cancel context.CancelFunc
	stream pb.ControllerService_ListenServer
}

type bearerToken struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Token     string `json:"token"`
}

func (s *ControllerService) authenticatePre(ctx context.Context) (*bearerToken, error) {
	logger := log.FromContext(ctx)
	encoded, err := BearerTokenFromContext(ctx)
	if err != nil {
		return nil, err
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		logger.Error(err, "failed to decode token", "encoded", encoded)
		return nil, status.Errorf(codes.InvalidArgument, "failed to decode token")
	}

	var token bearerToken

	err = json.Unmarshal(decoded, &token)
	if err != nil {
		logger.Error(err, "failed to unmarshal token", "decoded", decoded)
		return nil, status.Errorf(codes.InvalidArgument, "failed to unmarshal token")
	}

	return &token, nil
}

func (s *ControllerService) authenticateIdentity(ctx context.Context) (*jumpstarterdevv1alpha1.Identity, error) {
	logger := log.FromContext(ctx)
	token, err := s.authenticatePre(ctx)

	if err != nil {
		return nil, err
	}

	identityRef := types.NamespacedName{
		Namespace: token.Namespace,
		Name:      token.Name,
	}

	var identity jumpstarterdevv1alpha1.Identity

	logger.Info("authenticating identity", "identity", identityRef)
	if err := s.Client.Get(
		ctx,
		identityRef,
		&identity,
	); err != nil {
		logger.Error(err, "unable to get identity resource", "identity", identityRef)
		return nil, status.Errorf(codes.Internal, "unable to get identity resource")
	}

	for _, ref := range identity.Spec.Credentials {
		var secret corev1.Secret

		if err := s.Client.Get(ctx, types.NamespacedName{
			Namespace: ref.Namespace,
			Name:      ref.Name,
		}, &secret); err != nil {
			logger.Error(err, "unable to get secret resource", "identity", identityRef, "name", ref.Name)
			return nil, status.Errorf(codes.Internal, "unable to get secret resource")
		}

		if reference, ok := secret.Data["token"]; ok && slices.Equal(reference, []byte(token.Token)) {
			return &identity, nil
		}
	}

	logger.Error(nil, "no matching credential", "identity", identityRef)
	return nil, status.Errorf(codes.Unauthenticated, "no matching credential")
}

func (s *ControllerService) authenticateExporter(ctx context.Context) (*jumpstarterdevv1alpha1.Exporter, error) {
	token, err := s.authenticatePre(ctx)
	if err != nil {
		return nil, err
	}

	exporterRef := types.NamespacedName{
		Namespace: token.Namespace,
		Name:      token.Name,
	}

	var exporter jumpstarterdevv1alpha1.Exporter

	if err := s.Client.Get(
		ctx,
		exporterRef,
		&exporter,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "unable to get exporter resource")
	}

	for _, ref := range exporter.Spec.Credentials {
		var secret corev1.Secret

		if err := s.Client.Get(ctx, types.NamespacedName{
			Namespace: ref.Namespace,
			Name:      ref.Name,
		}, &secret); err != nil {
			return nil, status.Errorf(codes.Internal, "unable to get secret resource")
		}

		if reference, ok := secret.Data["token"]; ok && slices.Equal(reference, []byte(token.Token)) {
			return &exporter, nil
		}
	}

	return nil, status.Errorf(codes.Unauthenticated, "no matching credential")
}

func (s *ControllerService) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {

	logger := log.FromContext(ctx)

	logger.Info("Registering exporter", "request", req)

	exporter, err := s.authenticateExporter(ctx)
	if err != nil {
		logger.Error(err, "unable to authenticate exporter")
		return nil, err
	}

	exporter.Status.Conditions = []metav1.Condition{{
		Type:               "Available",
		Status:             "True",
		ObservedGeneration: exporter.GetGeneration(),
		LastTransitionTime: metav1.Time{Time: time.Now()},
		Reason:             "Register",
		Message:            "",
	}}

	devices := []jumpstarterdevv1alpha1.Device{}
	for _, device := range req.Reports {
		devices = append(devices, jumpstarterdevv1alpha1.Device{
			Uuid:   device.GetUuid(),
			Labels: device.GetLabels(),
		})
	}
	exporter.Status.Devices = devices

	if err := s.Status().Update(ctx, exporter); err != nil {
		logger.Error(err, "unable to update exporter status", "exporter", exporter)
		return nil, status.Errorf(codes.Internal, "unable to update exporter status: %s", err)
	}

	return &pb.RegisterResponse{}, nil
}

func (s *ControllerService) Unregister(
	ctx context.Context,
	req *pb.UnregisterRequest,
) (
	*pb.UnregisterResponse,
	error,
) {
	logger := log.FromContext(ctx)
	exporter, err := s.authenticateExporter(ctx)
	if err != nil {
		logger.Error(err, "unable to authenticate exporter")
		return nil, err
	}

	exporter.Status.Conditions = []metav1.Condition{{
		Type:               "Available",
		Status:             "False",
		ObservedGeneration: exporter.GetGeneration(),
		LastTransitionTime: metav1.Time{Time: time.Now()},
		Reason:             "Bye",
		Message:            req.GetReason(),
	}}

	if err := s.Status().Update(ctx, exporter); err != nil {
		logger.Error(err, "unable to update exporter status", "exporter", exporter.Name)
		return nil, status.Errorf(codes.Internal, "unable to update exporter status: %s", err)
	}

	logger.Info("exporter unregistered, updated as unavailable", "exporter", exporter.Name)

	return &pb.UnregisterResponse{}, nil
}

func (s *ControllerService) Listen(req *pb.ListenRequest, stream pb.ControllerService_ListenServer) error {
	ctx := stream.Context()
	logger := log.FromContext(ctx)

	exporter, err := s.authenticateExporter(ctx)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lctx := listenContext{
		cancel: cancel,
		stream: stream,
	}

	_, loaded := s.listen.LoadOrStore(exporter.GetName(), lctx)

	if loaded {
		// TODO: in this case we should probably end the previous listener
		//       and start the new one?
		logger.Error(nil, "exporter is already listening", "exporter", exporter.GetName())
		return status.Errorf(codes.AlreadyExists, "exporter is already listening")
	}

	defer s.listen.Delete(exporter.GetName())

	<-ctx.Done()
	return nil
}

func (s *ControllerService) Dial(ctx context.Context, req *pb.DialRequest) (*pb.DialResponse, error) {
	logger := log.FromContext(ctx)
	identity, err := s.authenticateIdentity(ctx)
	if err != nil {
		logger.Error(err, "unable to authenticate identity")
		return nil, err
	}

	// TODO: authorize user with Identity/Lease resource

	value, ok := s.listen.Load(req.GetUuid())
	if !ok {
		logger.Error(nil, "no matching listener", "client", identity.GetName(), "uuid", req.GetUuid())
		return nil, status.Errorf(codes.Unavailable, "no matching listener")
	}

	// TODO: put the name of the listener in the listen context, so we can
	//       log it here

	stream := uuid.NewUUID()

	audience := (&url.URL{
		// TODO should we use grpc scheme?
		Scheme: "https",
		Host:   routerEndpoint(),
		Path:   fmt.Sprintf("/stream/%s", stream),
	}).String()

	// TODO: make this configurable and requestable (with limits)
	expsecs := int64(3600)

	var tokenholder corev1.ServiceAccount

	tokenholderName := types.NamespacedName{
		Namespace: nameSpace,
		Name:      namePrefix + "tokenholder",
	}

	if err := s.Client.Get(ctx, tokenholderName, &tokenholder); err != nil {
		logger.Error(err, "failed to get tokenholder service account", "name", tokenholderName)
		return nil, status.Errorf(codes.Internal, "failed to get tokenholder service account")
	}

	tokenRequest := authv1.TokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: tokenholderName.Namespace,
			Name:      tokenholderName.Name,
		},
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expsecs,
		},
	}

	if err := s.SubResource("token").Create(ctx, &tokenholder, &tokenRequest); err != nil {
		logger.Error(err, "failed to issue stream token")
		return nil, status.Errorf(codes.Internal, "failed to issue stream token: %s", err)
	}

	// TODO: find best router from list
	endpoint := routerEndpoint()
	response := &pb.ListenResponse{
		RouterEndpoint: endpoint,
		RouterToken:    tokenRequest.Status.Token,
	}

	if err := value.(listenContext).stream.Send(response); err != nil {
		logger.Error(err, "failed to send listen response", "response", response)
		return nil, err
	}

	logger.Info("Client dial assigned stream ", "client", identity.GetName(), "stream", audience)
	return &pb.DialResponse{
		RouterEndpoint: endpoint,
		RouterToken:    tokenRequest.Status.Token,
	}, nil
}

func (s *ControllerService) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)

	server := grpc.NewServer()

	pb.RegisterControllerServiceServer(server, s)

	// Register reflection service on gRPC server.
	reflection.Register(server)

	listener, err := net.Listen("tcp", ":8082")
	if err != nil {
		return err
	}

	logger.Info("Starting Controller grpc service")

	go func() {
		<-ctx.Done()
		logger.Info("Stopping Controller gRPC service")
		server.Stop()
	}()

	return server.Serve(listener)
}

// SetupWithManager sets up the controller with the Manager.
func (s *ControllerService) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(s)
}
