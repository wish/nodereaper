package configmap

import (
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ConfigMap represents a configmap of some kind
type ConfigMap struct {
	clientset *kubernetes.Clientset
	namespace string
	name      string
	mu        *sync.Mutex
}

// New creates a new ConfigMap
func New(clientset *kubernetes.Clientset, namespace, name string) (*ConfigMap, error) {
	cmap := &ConfigMap{
		clientset,
		namespace,
		name,
		&sync.Mutex{},
	}
	_, err := cmap.getOrCreate()
	if err != nil {
		return nil, err
	}
	return cmap, nil
}

// Store stores the value at the given key
func (c *ConfigMap) Store(key string, value *string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmap, err := c.getOrCreate()
	if err != nil {
		return err
	}
	if value != nil {
		cmap.Data[key] = *value
	} else {
		delete(cmap.Data, key)
	}
	_, err = c.clientset.CoreV1().ConfigMaps(c.namespace).Update(cmap)
	return err
}

// Load gets the value of the given key, or nil if it doesn't exist
func (c *ConfigMap) Load(key string) (*string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmap, err := c.getOrCreate()
	if err != nil {
		return nil, err
	}
	if val, ok := cmap.Data[key]; ok {
		return &val, nil
	}
	return nil, nil
}

func (c *ConfigMap) getOrCreate() (*core_v1.ConfigMap, error) {
	cmap, err := c.clientset.CoreV1().ConfigMaps(c.namespace).Get(c.name, meta_v1.GetOptions{})
	if err != nil || cmap == nil {
		logrus.Infof("Failed to get configmap %v/%v, creating...", c.namespace, c.name)
		cmap, err = c.clientset.CoreV1().ConfigMaps(c.namespace).Create(&core_v1.ConfigMap{
			ObjectMeta: meta_v1.ObjectMeta{
				Name: c.name,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("Error creating configmap %v/%v: %v", c.namespace, c.name, err)
		}
	}
	if cmap.Data == nil {
		cmap.Data = make(map[string]string)
	}
	return cmap, nil
}
