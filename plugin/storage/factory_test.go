// Copyright (c) 2019 The Jaeger Authors.
// Copyright (c) 2018 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/metrics/fork"
	"github.com/jaegertracing/jaeger/internal/metricstest"
	"github.com/jaegertracing/jaeger/pkg/config"
	"github.com/jaegertracing/jaeger/pkg/metrics"
	"github.com/jaegertracing/jaeger/storage"
	"github.com/jaegertracing/jaeger/storage/dependencystore"
	depStoreMocks "github.com/jaegertracing/jaeger/storage/dependencystore/mocks"
	"github.com/jaegertracing/jaeger/storage/mocks"
	"github.com/jaegertracing/jaeger/storage/spanstore"
	spanStoreMocks "github.com/jaegertracing/jaeger/storage/spanstore/mocks"
)

func defaultCfg() FactoryConfig {
	return FactoryConfig{
		SpanWriterTypes:         []string{cassandraStorageType},
		SpanReaderType:          cassandraStorageType,
		DependenciesStorageType: cassandraStorageType,
		DownsamplingRatio:       1.0,
		DownsamplingHashSalt:    "",
	}
}

func TestNewFactory(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])
	assert.Equal(t, cassandraStorageType, f.SpanWriterTypes[0])
	assert.Equal(t, cassandraStorageType, f.SpanReaderType)
	assert.Equal(t, cassandraStorageType, f.DependenciesStorageType)

	f, err = NewFactory(FactoryConfig{
		SpanWriterTypes:         []string{cassandraStorageType, kafkaStorageType, badgerStorageType},
		SpanReaderType:          elasticsearchStorageType,
		DependenciesStorageType: memoryStorageType,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])
	assert.NotNil(t, f.factories[kafkaStorageType])
	assert.NotEmpty(t, f.factories[elasticsearchStorageType])
	assert.NotNil(t, f.factories[memoryStorageType])
	assert.Equal(t, []string{cassandraStorageType, kafkaStorageType, badgerStorageType}, f.SpanWriterTypes)
	assert.Equal(t, elasticsearchStorageType, f.SpanReaderType)
	assert.Equal(t, memoryStorageType, f.DependenciesStorageType)

	_, err = NewFactory(FactoryConfig{SpanWriterTypes: []string{"x"}, DependenciesStorageType: "y", SpanReaderType: "z"})
	require.Error(t, err)
	expected := "unknown storage type" // could be 'x' or 'y' since code iterates through map.
	assert.Equal(t, expected, err.Error()[0:len(expected)])

	require.NoError(t, f.Close())
}

func TestClose(t *testing.T) {
	storageType := "foo"
	err := fmt.Errorf("some error")
	f := Factory{
		factories: map[string]storage.Factory{
			storageType: &errorFactory{closeErr: err},
		},
		FactoryConfig: FactoryConfig{SpanWriterTypes: []string{storageType}},
	}
	require.EqualError(t, f.Close(), err.Error())
}

func TestInitialize(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])

	mock := new(mocks.Factory)
	f.factories[cassandraStorageType] = mock

	m := metrics.NullFactory
	l := zap.NewNop()
	mock.On("Initialize", m, l).Return(nil)
	require.NoError(t, f.Initialize(m, l))

	mock = new(mocks.Factory)
	f.factories[cassandraStorageType] = mock
	mock.On("Initialize", m, l).Return(errors.New("init-error"))
	require.EqualError(t, f.Initialize(m, l), "init-error")
}

func TestCreate(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])

	mock := new(mocks.Factory)
	f.factories[cassandraStorageType] = mock

	spanReader := new(spanStoreMocks.Reader)
	spanWriter := new(spanStoreMocks.Writer)
	depReader := new(depStoreMocks.Reader)

	mock.On("CreateSpanReader").Return(spanReader, errors.New("span-reader-error"))
	mock.On("CreateSpanWriter").Once().Return(spanWriter, errors.New("span-writer-error"))
	mock.On("CreateDependencyReader").Return(depReader, errors.New("dep-reader-error"))

	r, err := f.CreateSpanReader()
	assert.Equal(t, spanReader, r)
	require.EqualError(t, err, "span-reader-error")

	w, err := f.CreateSpanWriter()
	assert.Nil(t, w)
	require.EqualError(t, err, "span-writer-error")

	d, err := f.CreateDependencyReader()
	assert.Equal(t, depReader, d)
	require.EqualError(t, err, "dep-reader-error")

	_, err = f.CreateArchiveSpanReader()
	require.EqualError(t, err, "archive storage not supported")

	_, err = f.CreateArchiveSpanWriter()
	require.EqualError(t, err, "archive storage not supported")

	mock.On("CreateSpanWriter").Return(spanWriter, nil)
	m := metrics.NullFactory
	l := zap.NewNop()
	mock.On("Initialize", m, l).Return(nil)
	f.Initialize(m, l)
	w, err = f.CreateSpanWriter()
	require.NoError(t, err)
	assert.Equal(t, spanWriter, w)
}

func TestCreateDownsamplingWriter(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories[cassandraStorageType])
	mock := new(mocks.Factory)
	f.factories[cassandraStorageType] = mock
	spanWriter := new(spanStoreMocks.Writer)
	mock.On("CreateSpanWriter").Return(spanWriter, nil)

	m := metrics.NullFactory
	l := zap.NewNop()
	mock.On("Initialize", m, l).Return(nil)

	testParams := []struct {
		ratio      float64
		writerType string
	}{
		{0.5, "*spanstore.DownsamplingWriter"},
		{1.0, "*mocks.Writer"},
	}

	for _, param := range testParams {
		t.Run(param.writerType, func(t *testing.T) {
			f.DownsamplingRatio = param.ratio
			f.Initialize(m, l)
			newWriter, err := f.CreateSpanWriter()
			require.NoError(t, err)
			// Currently directly assertEqual doesn't work since DownsamplingWriter initializes with different
			// address for hashPool. The following workaround checks writer type instead
			assert.True(t, strings.HasPrefix(reflect.TypeOf(newWriter).String(), param.writerType))
		})
	}
}

func TestCreateMulti(t *testing.T) {
	cfg := defaultCfg()
	cfg.SpanWriterTypes = append(cfg.SpanWriterTypes, elasticsearchStorageType)
	f, err := NewFactory(cfg)
	require.NoError(t, err)

	mock := new(mocks.Factory)
	mock2 := new(mocks.Factory)
	f.factories[cassandraStorageType] = mock
	f.factories[elasticsearchStorageType] = mock2

	spanWriter := new(spanStoreMocks.Writer)
	spanWriter2 := new(spanStoreMocks.Writer)

	mock.On("CreateSpanWriter").Once().Return(spanWriter, errors.New("span-writer-error"))

	w, err := f.CreateSpanWriter()
	assert.Nil(t, w)
	require.EqualError(t, err, "span-writer-error")

	mock.On("CreateSpanWriter").Return(spanWriter, nil)
	mock2.On("CreateSpanWriter").Return(spanWriter2, nil)
	m := metrics.NullFactory
	l := zap.NewNop()
	mock.On("Initialize", m, l).Return(nil)
	mock2.On("Initialize", m, l).Return(nil)
	f.Initialize(m, l)
	w, err = f.CreateSpanWriter()
	require.NoError(t, err)
	assert.Equal(t, spanstore.NewCompositeWriter(spanWriter, spanWriter2), w)
}

func TestCreateArchive(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories[cassandraStorageType])

	mock := &struct {
		mocks.Factory
		mocks.ArchiveFactory
	}{}
	f.factories[cassandraStorageType] = mock

	archiveSpanReader := new(spanStoreMocks.Reader)
	archiveSpanWriter := new(spanStoreMocks.Writer)

	mock.ArchiveFactory.On("CreateArchiveSpanReader").Return(archiveSpanReader, errors.New("archive-span-reader-error"))
	mock.ArchiveFactory.On("CreateArchiveSpanWriter").Return(archiveSpanWriter, errors.New("archive-span-writer-error"))

	ar, err := f.CreateArchiveSpanReader()
	assert.Equal(t, archiveSpanReader, ar)
	require.EqualError(t, err, "archive-span-reader-error")

	aw, err := f.CreateArchiveSpanWriter()
	assert.Equal(t, archiveSpanWriter, aw)
	require.EqualError(t, err, "archive-span-writer-error")
}

func TestCreateError(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])
	delete(f.factories, cassandraStorageType)

	expectedErr := "no cassandra backend registered for span store"
	// scope the vars to avoid bugs in the test
	{
		r, err := f.CreateSpanReader()
		assert.Nil(t, r)
		require.EqualError(t, err, expectedErr)
	}

	{
		w, err := f.CreateSpanWriter()
		assert.Nil(t, w)
		require.EqualError(t, err, expectedErr)
	}

	{
		d, err := f.CreateDependencyReader()
		assert.Nil(t, d)
		require.EqualError(t, err, expectedErr)
	}

	{
		r, err := f.CreateArchiveSpanReader()
		assert.Nil(t, r)
		require.EqualError(t, err, expectedErr)
	}

	{
		w, err := f.CreateArchiveSpanWriter()
		assert.Nil(t, w)
		require.EqualError(t, err, expectedErr)
	}
}

func TestAllSamplingStorageTypes(t *testing.T) {
	assert.Equal(t, []string{"cassandra", "memory", "badger"}, AllSamplingStorageTypes())
}

func TestCreateSamplingStoreFactory(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])

	// if not specified sampling store is chosen from available factories
	ssFactory, err := f.CreateSamplingStoreFactory()
	assert.Equal(t, f.factories[cassandraStorageType], ssFactory)
	require.NoError(t, err)

	// if not specified and there's no compatible factories then return nil
	delete(f.factories, cassandraStorageType)
	ssFactory, err = f.CreateSamplingStoreFactory()
	assert.Nil(t, ssFactory)
	require.NoError(t, err)

	// if an incompatible factory is specified return err
	cfg := defaultCfg()
	cfg.SamplingStorageType = "elasticsearch"
	f, err = NewFactory(cfg)
	require.NoError(t, err)
	ssFactory, err = f.CreateSamplingStoreFactory()
	assert.Nil(t, ssFactory)
	require.EqualError(t, err, "storage factory of type elasticsearch does not support sampling store")

	// if a compatible factory is specified then return it
	cfg.SamplingStorageType = "cassandra"
	f, err = NewFactory(cfg)
	require.NoError(t, err)
	ssFactory, err = f.CreateSamplingStoreFactory()
	assert.Equal(t, ssFactory, f.factories["cassandra"])
	require.NoError(t, err)
}

type configurable struct {
	mocks.Factory
	flagSet *flag.FlagSet
	viper   *viper.Viper
	logger  *zap.Logger
}

// AddFlags implements plugin.Configurable
func (f *configurable) AddFlags(flagSet *flag.FlagSet) {
	f.flagSet = flagSet
}

// InitFromViper implements plugin.Configurable
func (f *configurable) InitFromViper(v *viper.Viper, logger *zap.Logger) {
	f.viper = v
	f.logger = logger
}

func TestConfigurable(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)
	assert.NotEmpty(t, f.factories)
	assert.NotEmpty(t, f.factories[cassandraStorageType])

	mock := new(configurable)
	f.factories[cassandraStorageType] = mock

	fs := new(flag.FlagSet)
	v := viper.New()

	f.AddFlags(fs)
	f.InitFromViper(v, zap.NewNop())

	assert.Equal(t, fs, mock.flagSet)
	assert.Equal(t, v, mock.viper)
}

func TestParsingDownsamplingRatio(t *testing.T) {
	f := Factory{}
	v, command := config.Viperize(f.AddPipelineFlags)
	err := command.ParseFlags([]string{
		"--downsampling.ratio=1.5",
		"--downsampling.hashsalt=jaeger",
	})
	require.NoError(t, err)
	f.InitFromViper(v, zap.NewNop())

	assert.Equal(t, 1.0, f.FactoryConfig.DownsamplingRatio)
	assert.Equal(t, "jaeger", f.FactoryConfig.DownsamplingHashSalt)

	err = command.ParseFlags([]string{
		"--downsampling.ratio=0.5",
	})
	require.NoError(t, err)
	f.InitFromViper(v, zap.NewNop())
	assert.Equal(t, 0.5, f.FactoryConfig.DownsamplingRatio)
}

func TestDefaultDownsamplingWithAddFlags(t *testing.T) {
	f := Factory{}
	v, command := config.Viperize(f.AddFlags)
	err := command.ParseFlags([]string{})
	require.NoError(t, err)
	f.InitFromViper(v, zap.NewNop())

	assert.Equal(t, defaultDownsamplingRatio, f.FactoryConfig.DownsamplingRatio)
	assert.Equal(t, defaultDownsamplingHashSalt, f.FactoryConfig.DownsamplingHashSalt)

	err = command.ParseFlags([]string{
		"--downsampling.ratio=0.5",
	})
	require.Error(t, err)
}

func TestPublishOpts(t *testing.T) {
	f, err := NewFactory(defaultCfg())
	require.NoError(t, err)

	baseMetrics := metricstest.NewFactory(time.Second)
	forkFactory := metricstest.NewFactory(time.Second)
	metricsFactory := fork.New("internal", forkFactory, baseMetrics)
	f.metricsFactory = metricsFactory

	// This method is called inside factory.Initialize method
	f.publishOpts()

	forkFactory.AssertGaugeMetrics(t, metricstest.ExpectedMetric{
		Name:  "internal." + downsamplingRatio,
		Value: int(f.DownsamplingRatio),
	})
	forkFactory.AssertGaugeMetrics(t, metricstest.ExpectedMetric{
		Name:  "internal." + spanStorageType + "-" + f.SpanReaderType,
		Value: 1,
	})
}

type errorFactory struct {
	closeErr error
}

var (
	_ storage.Factory = (*errorFactory)(nil)
	_ io.Closer       = (*errorFactory)(nil)
)

func (e errorFactory) Initialize(metricsFactory metrics.Factory, logger *zap.Logger) error {
	panic("implement me")
}

func (e errorFactory) CreateSpanReader() (spanstore.Reader, error) {
	panic("implement me")
}

func (e errorFactory) CreateSpanWriter() (spanstore.Writer, error) {
	panic("implement me")
}

func (e errorFactory) CreateDependencyReader() (dependencystore.Reader, error) {
	panic("implement me")
}

func (e errorFactory) Close() error {
	return e.closeErr
}
