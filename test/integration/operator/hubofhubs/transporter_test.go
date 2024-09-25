package hubofhubs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kafkav1beta2 "github.com/RedHatInsights/strimzi-client-go/apis/kafka.strimzi.io/v1beta2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stolostron/multicluster-global-hub/operator/api/operator/v1alpha4"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/config"
	operatortrans "github.com/stolostron/multicluster-global-hub/operator/pkg/controllers/hubofhubs/transporter"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/controllers/hubofhubs/transporter/protocol"
	operatorutils "github.com/stolostron/multicluster-global-hub/operator/pkg/utils"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/transport"
	"github.com/stolostron/multicluster-global-hub/pkg/utils"
	testutils "github.com/stolostron/multicluster-global-hub/test/integration/utils"
)

// go test ./test/integration/operator/hubofhubs -ginkgo.focus "transporter" -v
var _ = Describe("transporter", Ordered, func() {
	var mgh *v1alpha4.MulticlusterGlobalHub
	var namespace string
	BeforeAll(func() {
		namespace = fmt.Sprintf("namespace-%s", rand.String(6))
		mghName := "test-mgh"

		// mgh
		Expect(runtimeClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
		mgh = &v1alpha4.MulticlusterGlobalHub{
			ObjectMeta: metav1.ObjectMeta{
				Name:      mghName,
				Namespace: namespace,
			},
			Spec: v1alpha4.MulticlusterGlobalHubSpec{
				EnableMetrics: true,
				DataLayer: v1alpha4.DataLayerConfig{
					Postgres: v1alpha4.PostgresConfig{
						Retention: "2y",
					},
				},
			},
		}
		Expect(runtimeClient.Create(ctx, mgh)).To(Succeed())
		Expect(runtimeClient.Get(ctx, client.ObjectKeyFromObject(mgh), mgh)).To(Succeed())
	})

	It("should generate the transport connection in BYO case", func() {
		// transport
		err := CreateTestSecretTransport(runtimeClient, mgh.Namespace)
		Expect(err).To(Succeed())
		// update the transport protocol configuration
		err = config.SetTransportConfig(ctx, runtimeClient, mgh)
		Expect(err).To(Succeed())

		// verify the type
		Expect(config.TransporterProtocol()).To(Equal(transport.SecretTransporter))
		Expect(config.GetSpecTopic()).To(Equal("gh-spec"))
		Expect(config.GetRawStatusTopic()).To(Equal("gh-status"))

		// err = config.SetMulticlusterGlobalHubConfig(ctx, mgh, nil)
		Expect(err).To(Succeed())
		reconciler := operatortrans.NewTransportReconciler(runtimeManager)

		Eventually(func() error {
			err = reconciler.Reconcile(ctx, mgh)
			if err != nil {
				return err
			}

			// the connection is generated
			conn := config.GetTransporterConn()
			if conn == nil {
				return fmt.Errorf("the connection should be generated by transport secret")
			}
			utils.PrettyPrint(conn)
			return nil
		}, 10*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())

		// delete the transport secret
		err = runtimeClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      constants.GHTransportSecretName,
				Namespace: mgh.Namespace,
			},
		})
		Expect(err).To(Succeed())
	})

	It("should generate the transport connection in strimzi transport", func() {
		config.SetTransporter(nil)
		config.SetTransporterConn(nil)
		// the crd resources is ready
		config.SetKafkaResourceReady(true)

		// update the transport protocol configuration, topic
		err := config.SetMulticlusterGlobalHubConfig(ctx, mgh, nil, nil)
		Expect(err).To(Succeed())
		err = config.SetTransportConfig(ctx, runtimeClient, mgh)
		Expect(err).To(Succeed())

		Expect(config.TransporterProtocol()).To(Equal(transport.StrimziTransporter))
		Expect(config.GetSpecTopic()).To(Equal("gh-spec"))
		Expect(config.GetRawStatusTopic()).To(Equal("gh-status"))

		reconciler := operatortrans.NewTransportReconciler(runtimeManager)

		// blocking until get the connection
		go func() {
			err = reconciler.Reconcile(ctx, mgh)
			for err != nil {
				fmt.Println("reconciler error, retrying ...", err.Error())
				time.Sleep(1 * time.Second)

				_ = config.SetMulticlusterGlobalHubConfig(ctx, mgh, nil, nil)
				err = reconciler.Reconcile(ctx, mgh)
			}
		}()

		// the subscription
		Eventually(func() error {
			sub, err := operatorutils.GetSubscriptionByName(ctx, runtimeClient, namespace, protocol.DefaultKafkaSubName)
			if err != nil {
				return err
			}
			if sub == nil {
				return fmt.Errorf("should get the subscription %s", protocol.DefaultKafkaSubName)
			}

			return nil
		}, 20*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())

		// the kafka cluster
		Eventually(func() error {
			kafka := &kafkav1beta2.Kafka{}
			err := runtimeClient.Get(ctx, types.NamespacedName{
				Name:      protocol.KafkaClusterName,
				Namespace: mgh.Namespace,
			}, kafka)
			if err != nil {
				return err
			}
			return nil
		}, 10*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())

		// update the kafka resource to make it ready
		err = UpdateKafkaClusterReady(runtimeClient, mgh.Namespace)
		Expect(err).To(Succeed())

		// verify the metrics resources and pod monitor
		Eventually(func() error {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kafka-metrics",
					Namespace: mgh.Namespace,
				},
			}
			err = runtimeClient.Get(ctx, client.ObjectKeyFromObject(cm), cm)
			if err != nil {
				return err
			}
			podMonitor := &promv1.PodMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "kafka-resources-metrics",
					Namespace: mgh.Namespace,
				},
			}
			err = runtimeClient.Get(ctx, client.ObjectKeyFromObject(podMonitor), podMonitor)
			if err != nil {
				return err
			}
			return nil
		}, 10*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())

		Eventually(func() error {
			// the connection for manager is generated
			conn := config.GetTransporterConn()
			if conn == nil {
				return fmt.Errorf("the strimzi connection should not be nil")
			}
			// get the conn by transporter
			tran := config.GetTransporter()
			agentConn, err := tran.GetConnCredential("hub1")
			if err != nil {
				return err
			}
			if agentConn == nil {
				return fmt.Errorf("the strimzi connection for hub1 should not be nil")
			}
			return nil
		}, 20*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())
	})

	It("should pass the strimzi transport configuration", func() {
		trans := protocol.NewStrimziTransporter(
			runtimeManager,
			mgh,
			protocol.WithCommunity(false),
			protocol.WithNamespacedName(types.NamespacedName{
				Name:      protocol.KafkaClusterName,
				Namespace: mgh.Namespace,
			}),
		)

		customCPURequest := "1m"
		customCPULimit := "2m"
		customMemoryRequest := "1Mi"
		customMemoryLimit := "2Mi"
		mgh.Spec.AdvancedConfig = &v1alpha4.AdvancedConfig{
			Kafka: &v1alpha4.CommonSpec{
				Resources: &v1alpha4.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceCPU):    resource.MustParse(customCPULimit),
						corev1.ResourceName(corev1.ResourceMemory): resource.MustParse(customMemoryLimit),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceMemory): resource.MustParse(customMemoryRequest),
						corev1.ResourceName(corev1.ResourceCPU):    resource.MustParse(customCPURequest),
					},
				},
			},
			Zookeeper: &v1alpha4.CommonSpec{
				Resources: &v1alpha4.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceCPU):    resource.MustParse(customCPULimit),
						corev1.ResourceName(corev1.ResourceMemory): resource.MustParse(customMemoryLimit),
					},
					Requests: corev1.ResourceList{
						corev1.ResourceName(corev1.ResourceMemory): resource.MustParse(customMemoryRequest),
						corev1.ResourceName(corev1.ResourceCPU):    resource.MustParse(customCPURequest),
					},
				},
			},
		}
		mgh.Spec.ImagePullSecret = "mgh-image-pull"

		err, updated := trans.CreateUpdateKafkaCluster(mgh)
		Expect(err).To(Succeed())
		Expect(updated).To(BeTrue())

		mgh.Spec.NodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
		}
		mgh.Spec.Tolerations = []corev1.Toleration{
			{
				Key:      "node-role.kubernetes.io/worker",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}
		Eventually(func() error {
			err, _ = trans.CreateUpdateKafkaCluster(mgh)
			return err
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		mgh.Spec.ImagePullSecret = "mgh-image-pull-update"
		Eventually(func() error {
			err, _ = trans.CreateUpdateKafkaCluster(mgh)
			if err != nil {
				return err
			}
			cluster := &kafkav1beta2.Kafka{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: mgh.Namespace,
					Name:      protocol.KafkaClusterName,
				},
			}
			err = runtimeClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
			if err != nil {
				return err
			}
			pullSecrets := cluster.Spec.Kafka.Template.Pod.ImagePullSecrets
			if len(pullSecrets) == 0 {
				return fmt.Errorf("should update the image pull secret")
			}
			if *pullSecrets[0].Name != mgh.Spec.ImagePullSecret {
				return fmt.Errorf("should get the image pull secret %s, but got %s", mgh.Spec.ImagePullSecret,
					*pullSecrets[0].Name)
			}
			return nil
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		kafka := &kafkav1beta2.Kafka{}
		err = runtimeClient.Get(ctx, types.NamespacedName{
			Namespace: mgh.Namespace,
			Name:      protocol.KafkaClusterName,
		}, kafka)
		Expect(err).To(Succeed())

		Expect(kafka.Spec.Kafka.Template.Pod.Affinity.NodeAffinity).NotTo(BeNil())
		Expect(kafka.Spec.Kafka.Template.Pod.Tolerations).NotTo(BeEmpty())
		Expect(kafka.Spec.Kafka.Template.Pod.ImagePullSecrets).NotTo(BeEmpty())

		Expect(string(kafka.Spec.Kafka.Resources.Requests.Raw)).To(Equal(`{"cpu":"1m","memory":"1Mi"}`))
		Expect(string(kafka.Spec.Kafka.Resources.Limits.Raw)).To(Equal(`{"cpu":"2m","memory":"2Mi"}`))

		Expect(kafka.Spec.Zookeeper.Template.Pod.Affinity.NodeAffinity).NotTo(BeNil())
		Expect(kafka.Spec.Zookeeper.Template.Pod.Tolerations).NotTo(BeEmpty())
		Expect(kafka.Spec.Zookeeper.Template.Pod.ImagePullSecrets).NotTo(BeEmpty())

		Expect(string(kafka.Spec.Zookeeper.Resources.Requests.Raw)).To(Equal(`{"cpu":"1m","memory":"1Mi"}`))
		Expect(string(kafka.Spec.Zookeeper.Resources.Limits.Raw)).To(Equal(`{"cpu":"2m","memory":"2Mi"}`))

		Expect(kafka.Spec.EntityOperator.Template.Pod.Affinity.NodeAffinity).NotTo(BeNil())
		Expect(kafka.Spec.EntityOperator.Template.Pod.Tolerations).NotTo(BeEmpty())
		Expect(kafka.Spec.EntityOperator.Template.Pod.ImagePullSecrets).NotTo(BeEmpty())

		mgh.Spec.NodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
			"topology.kubernetes.io/zone":    "east1",
		}
		mgh.Spec.Tolerations = []corev1.Toleration{
			{
				Key:      "node.kubernetes.io/not-ready",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
			{
				Key:      "node-role.kubernetes.io/worker",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}
		Eventually(func() error {
			err, updated = trans.CreateUpdateKafkaCluster(mgh)
			if err != nil {
				return err
			}
			if !updated {
				return fmt.Errorf("the kafka cluster should updated")
			}
			return nil
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		kafka = &kafkav1beta2.Kafka{}
		err = runtimeClient.Get(ctx, types.NamespacedName{
			Namespace: mgh.Namespace,
			Name:      protocol.KafkaClusterName,
		}, kafka)
		Expect(err).To(Succeed())

		entityOperatorToleration, _ := json.Marshal(kafka.Spec.EntityOperator.Template.Pod.Tolerations)
		kafkaToleration, _ := json.Marshal(kafka.Spec.Kafka.Template.Pod.Tolerations)
		zookeeperToleration, _ := json.Marshal(kafka.Spec.Zookeeper.Template.Pod.Tolerations)
		entityOperatorNodeAffinity, _ := json.Marshal(kafka.Spec.EntityOperator.Template.Pod.Affinity.NodeAffinity)
		kafkaNodeAffinity, _ := json.Marshal(kafka.Spec.Kafka.Template.Pod.Affinity.NodeAffinity)
		zookeeperNodeAffinity, _ := json.Marshal(kafka.Spec.Zookeeper.Template.Pod.Affinity.NodeAffinity)
		toleration := `[{"effect":"NoSchedule","key":"node.kubernetes.io/not-ready","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/worker","operator":"Exists"}]`

		Expect(string(entityOperatorToleration)).To(Equal(toleration))
		Expect(string(kafkaToleration)).To(Equal(toleration))
		Expect(string(zookeeperToleration)).To(Equal(toleration))

		// cannot compare the string, because the order is random
		Expect(string(entityOperatorNodeAffinity)).To(ContainSubstring("node-role.kubernetes.io/worker"))
		Expect(string(entityOperatorNodeAffinity)).To(ContainSubstring("topology.kubernetes.io/zone"))
		Expect(string(kafkaNodeAffinity)).To(ContainSubstring("node-role.kubernetes.io/worker"))
		Expect(string(kafkaNodeAffinity)).To(ContainSubstring("topology.kubernetes.io/zone"))
		Expect(string(zookeeperNodeAffinity)).To(ContainSubstring("node-role.kubernetes.io/worker"))
		Expect(string(zookeeperNodeAffinity)).To(ContainSubstring("topology.kubernetes.io/zone"))

		// simulate to create a cluster named: hub1
		clusterName := "hub1"

		// user - round 1
		userName, err := trans.EnsureUser(clusterName)
		Expect(err).To(Succeed())
		Expect(config.GetKafkaUserName(clusterName)).To(Equal(userName))

		kafkaUser := &kafkav1beta2.KafkaUser{
			ObjectMeta: metav1.ObjectMeta{
				Name:      userName,
				Namespace: mgh.Namespace,
			},
		}

		err = runtimeClient.Get(ctx, client.ObjectKeyFromObject(kafkaUser), kafkaUser)
		Expect(err).To(Succeed())
		Expect(3).To(Equal(len(kafkaUser.Spec.Authorization.Acls)))

		// topic: create
		clusterTopic, err := trans.EnsureTopic(clusterName)
		Expect(err).To(Succeed())
		Expect("gh-spec").To(Equal(clusterTopic.SpecTopic))
		Expect(config.GetStatusTopic(clusterName)).To(Equal(clusterTopic.StatusTopic))

		// topic: update
		_, err = trans.EnsureTopic(clusterName)
		if !errors.IsAlreadyExists(err) {
			Expect(err).To(Succeed())
		}

		err = trans.Prune(clusterName)
		Expect(err).To(Succeed())
	})

	AfterAll(func() {
		Eventually(func() error {
			return testutils.DeleteMgh(ctx, runtimeClient, mgh)
		}, 10*time.Second, 100*time.Millisecond).ShouldNot(HaveOccurred())

		err := runtimeClient.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})
		Expect(err).To(Succeed())
	})
})

func UpdateKafkaClusterReady(c client.Client, ns string) error {
	kafkaVersion := "3.5.0"
	kafkaClusterName := "kafka"
	globalHubKafkaUser := "global-hub-kafka-user"
	clientCa := "kafka-clients-ca-cert"
	clientCaCert := "kafka-clients-ca"

	readyCondition := "Ready"
	trueCondition := "True"
	bootServer := "kafka-kafka-bootstrap.multicluster-global-hub.svc:9092"
	statusClusterId := "MXpoZsJTRD2DDiVUh3Rsqg"

	statusKafkaCluster := &kafkav1beta2.Kafka{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      kafkaClusterName,
		},
		Spec: &kafkav1beta2.KafkaSpec{
			Kafka: kafkav1beta2.KafkaSpecKafka{
				Replicas: 1,
				Storage: kafkav1beta2.KafkaSpecKafkaStorage{
					Type: "ephemeral",
				},
				Listeners: []kafkav1beta2.KafkaSpecKafkaListenersElem{
					{
						Name: "plain",
						Port: 9092,
						Type: "internal",
					},
				},
				Config: &apiextensions.JSON{Raw: []byte(`{
"default.replication.factor": 3
}`)},
				Version: &kafkaVersion,
			},
			Zookeeper: kafkav1beta2.KafkaSpecZookeeper{
				Replicas: 1,
				Storage: kafkav1beta2.KafkaSpecZookeeperStorage{
					Type: "ephemeral",
				},
			},
		},
		Status: &kafkav1beta2.KafkaStatus{
			ClusterId: &statusClusterId,
			Listeners: []kafkav1beta2.KafkaStatusListenersElem{
				{
					BootstrapServers: &bootServer,
				},
				{
					BootstrapServers: &bootServer,
					Certificates: []string{
						"cert",
					},
				},
			},
			Conditions: []kafkav1beta2.KafkaStatusConditionsElem{
				{
					Type:   &readyCondition,
					Status: &trueCondition,
				},
			},
		},
	}

	err := wait.PollImmediate(1*time.Second, 1*time.Minute, func() (bool, error) {
		existkafkaCluster := &kafkav1beta2.Kafka{}
		err := c.Get(context.Background(), types.NamespacedName{
			Name:      kafkaClusterName,
			Namespace: ns,
		}, existkafkaCluster)
		if err != nil {
			if errors.IsNotFound(err) {
				if e := c.Create(context.Background(), statusKafkaCluster); e != nil {
					klog.Errorf("Failed to create kafka cluster, error: %v", e)
					return false, nil
				}
			} else {
				klog.Errorf("Failed to get Kafka cluster, error:%v", err)
			}
			return false, nil
		}
		existkafkaCluster.Status = &kafkav1beta2.KafkaStatus{
			Listeners: []kafkav1beta2.KafkaStatusListenersElem{
				{
					BootstrapServers: &bootServer,
				},
				{
					BootstrapServers: &bootServer,
					Certificates: []string{
						"cert",
					},
				},
			},
			Conditions: []kafkav1beta2.KafkaStatusConditionsElem{
				{
					Type:   &readyCondition,
					Status: &trueCondition,
				},
			},
		}
		err = c.Status().Update(context.Background(), existkafkaCluster)
		if err != nil {
			klog.Errorf("Failed to update Kafka cluster, error:%v", err)
			return false, nil
		}
		return true, nil
	})

	err = createSecret(c, ns, globalHubKafkaUser, map[string][]byte{
		"user.crt": []byte("usercrt"),
		"user.key": []byte("userkey"),
	})
	if err != nil {
		return err
	}

	err = createSecret(c, ns, clientCa, map[string][]byte{
		"ca.key": []byte("cakey"),
	})
	if err != nil {
		return err
	}

	err = createSecret(c, ns, clientCaCert, map[string][]byte{
		"ca.crt": []byte("cacert"),
	})
	if err != nil {
		return err
	}
	return nil
}

func createSecret(c client.Client, ns, name string, data map[string][]byte) error {
	clientCaCertSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Data: data,
	}
	err := c.Get(context.Background(), client.ObjectKeyFromObject(clientCaCertSecret), clientCaCertSecret)
	if errors.IsNotFound(err) {
		e := c.Create(context.Background(), clientCaCertSecret)
		if e != nil {
			return e
		}
	} else if err != nil {
		return err
	}
	return nil
}