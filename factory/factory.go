package factory

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync/atomic"
)

var (
	TagName    = "factory"
	emptyValue = reflect.Value{}
)

type Factory struct {
	model        interface{}
	numField     int
	rt           reflect.Type
	rv           *reflect.Value
	attrGens     []*attrGenerator
	nameIndexMap map[string]int // pair for attribute name and field index.
	isPtr        bool
	onCreate     func(Args) error
}

type Args interface {
	Instance() interface{}
	Parent() Args
	Context() context.Context
	UpdateContext(context.Context)
	pipeline(int) *pipeline
}

type argsStruct struct {
	ctx context.Context
	rv  *reflect.Value
	pl  *pipeline
}

// Instance returns a object to which the generator declared just before is applied
func (args *argsStruct) Instance() interface{} {
	return args.rv.Interface()
}

// Parent returns a parent argument if current factory is a subfactory of parent
func (args *argsStruct) Parent() Args {
	if args.pl == nil {
		return nil
	}
	return args.pl.parent
}

func (args *argsStruct) pipeline(num int) *pipeline {
	if args.pl == nil {
		return newPipeline(num)
	}
	return args.pl
}

func (args *argsStruct) Context() context.Context {
	return args.ctx
}

func (args *argsStruct) UpdateContext(ctx context.Context) {
	args.ctx = ctx
}

type Stacks []*int64

func (st *Stacks) Size(idx int) int64 {
	return *(*st)[idx]
}

// Set method is not goroutine safe.
func (st *Stacks) Set(idx, val int) {
	var ini int64 = 0
	(*st)[idx] = &ini
	atomic.StoreInt64((*st)[idx], int64(val))
}

func (st *Stacks) Push(idx, delta int) {
	atomic.AddInt64((*st)[idx], int64(delta))
}

func (st *Stacks) Pop(idx, delta int) {
	atomic.AddInt64((*st)[idx], -int64(delta))
}

func (st *Stacks) Next(idx int) bool {
	st.Pop(idx, 1)
	return *(*st)[idx] >= 0
}

func (st *Stacks) Has(idx int) bool {
	return (*st)[idx] != nil
}

type pipeline struct {
	stacks Stacks
	parent Args
}

func newPipeline(size int) *pipeline {
	return &pipeline{stacks: make(Stacks, size)}
}

func (pl *pipeline) Next(args Args) *pipeline {
	npl := &pipeline{}
	npl.parent = args
	npl.stacks = make(Stacks, len(pl.stacks))
	for i, sptr := range pl.stacks {
		if sptr != nil {
			stack := *sptr
			npl.stacks[i] = &stack
		}
	}
	return npl
}

// NewFactory returns a new factory for specified model class
// Each generator is applied in the order in which they are declared
func NewFactory(model interface{}) *Factory {
	fa := &Factory{}
	fa.model = model
	fa.nameIndexMap = make(map[string]int)

	fa.init()
	return fa
}

type attrGenerator struct {
	genFunc func(Args) (interface{}, error)
	key     string
	value   interface{}
	isNil   bool
}

func (fa *Factory) init() {
	rt := reflect.TypeOf(fa.model)
	rv := reflect.ValueOf(fa.model)

	fa.isPtr = rt.Kind() == reflect.Ptr

	if fa.isPtr {
		rt = rt.Elem()
		rv = rv.Elem()
	}

	fa.numField = rv.NumField()

	for i := 0; i < fa.numField; i++ {
		tf := rt.Field(i)
		vf := rv.Field(i)
		ag := &attrGenerator{}

		if !vf.CanSet() || (tf.Type.Kind() == reflect.Ptr && vf.IsNil()) {
			ag.isNil = true
		} else {
			ag.value = vf.Interface()
		}

		attrName := getAttrName(tf, TagName)
		ag.key = attrName
		fa.nameIndexMap[attrName] = i
		fa.attrGens = append(fa.attrGens, ag)
	}

	fa.rt = rt
	fa.rv = &rv
}

func (fa *Factory) modelName() string {
	return fa.rt.Name()
}

func (fa *Factory) Attr(name string, gen func(Args) (interface{}, error)) *Factory {
	idx := fa.checkIdx(name)
	fa.attrGens[idx].genFunc = gen
	return fa
}

func (fa *Factory) SeqInt(name string, gen func(int) (interface{}, error)) *Factory {
	idx := fa.checkIdx(name)
	var seq int64 = 0
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		new := atomic.AddInt64(&seq, 1)
		return gen(int(new))
	}
	return fa
}

func (fa *Factory) SeqInt64(name string, gen func(int64) (interface{}, error)) *Factory {
	idx := fa.checkIdx(name)
	var seq int64 = 0
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		new := atomic.AddInt64(&seq, 1)
		return gen(new)
	}
	return fa
}

func (fa *Factory) SeqString(name string, gen func(string) (interface{}, error)) *Factory {
	idx := fa.checkIdx(name)
	var seq int64 = 0
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		new := atomic.AddInt64(&seq, 1)
		return gen(strconv.FormatInt(new, 10))
	}
	return fa
}

func (fa *Factory) SubFactory(name string, sub *Factory) *Factory {
	idx := fa.checkIdx(name)
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		pipeline := args.pipeline(fa.numField)
		ret, err := sub.create(args.Context(), nil, pipeline.Next(args))
		if err != nil {
			return nil, err
		}
		return ret, nil
	}
	return fa
}

func (fa *Factory) SubSliceFactory(name string, sub *Factory, getSize func() int) *Factory {
	idx := fa.checkIdx(name)
	tp := fa.rt.Field(idx).Type
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		size := getSize()
		pipeline := args.pipeline(fa.numField)
		sv := reflect.MakeSlice(tp, size, size)
		for i := 0; i < size; i++ {
			ret, err := sub.create(args.Context(), nil, pipeline.Next(args))
			if err != nil {
				return nil, err
			}
			sv.Index(i).Set(reflect.ValueOf(ret))
		}
		return sv.Interface(), nil
	}
	return fa
}

func (fa *Factory) SubRecursiveFactory(name string, sub *Factory, getLimit func() int) *Factory {
	idx := fa.checkIdx(name)
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		pl := args.pipeline(fa.numField)
		if !pl.stacks.Has(idx) {
			pl.stacks.Set(idx, getLimit())
		}
		if pl.stacks.Next(idx) {
			ret, err := sub.create(args.Context(), nil, pl.Next(args))
			if err != nil {
				return nil, err
			}
			return ret, nil
		}
		return nil, nil
	}
	return fa
}

func (fa *Factory) SubRecursiveSliceFactory(name string, sub *Factory, getSize, getLimit func() int) *Factory {
	idx := fa.checkIdx(name)
	tp := fa.rt.Field(idx).Type
	fa.attrGens[idx].genFunc = func(args Args) (interface{}, error) {
		pl := args.pipeline(fa.numField)
		if !pl.stacks.Has(idx) {
			pl.stacks.Set(idx, getLimit())
		}
		if pl.stacks.Next(idx) {
			size := getSize()
			sv := reflect.MakeSlice(tp, size, size)
			for i := 0; i < size; i++ {
				ret, err := sub.create(args.Context(), nil, pl.Next(args))
				if err != nil {
					return nil, err
				}
				sv.Index(i).Set(reflect.ValueOf(ret))
			}
			return sv.Interface(), nil
		}
		return nil, nil
	}
	return fa
}

// OnCreate registers a callback on object creation.
// If callback function returns error, object creation is failed.
func (fa *Factory) OnCreate(cb func(Args) error) *Factory {
	fa.onCreate = cb
	return fa
}

func (fa *Factory) checkIdx(name string) int {
	idx, ok := fa.nameIndexMap[name]
	if !ok {
		panic("No such attribute name: " + name)
	}
	return idx
}

func (fa *Factory) Create() (interface{}, error) {
	return fa.CreateWithOption(nil)
}

func (fa *Factory) CreateWithOption(opt map[string]interface{}) (interface{}, error) {
	return fa.create(context.Background(), opt, nil)
}

func (fa *Factory) CreateWithContext(ctx context.Context) (interface{}, error) {
	return fa.create(ctx, nil, nil)
}

func (fa *Factory) CreateWithContextAndOption(ctx context.Context, opt map[string]interface{}) (interface{}, error) {
	return fa.create(ctx, opt, nil)
}

func (fa *Factory) MustCreate() interface{} {
	return fa.MustCreateWithOption(nil)
}

func (fa *Factory) MustCreateWithOption(opt map[string]interface{}) interface{} {
	return fa.MustCreateWithContextAndOption(context.Background(), opt)
}

func (fa *Factory) MustCreateWithContextAndOption(ctx context.Context, opt map[string]interface{}) interface{} {
	inst, err := fa.CreateWithContextAndOption(ctx, opt)
	if err != nil {
		panic(err)
	}
	return inst
}

/*
Bind values of a new objects to a pointer to struct.

ptr: a pointer to struct
*/
func (fa *Factory) Construct(ptr interface{}) error {
	return fa.ConstructWithOption(ptr, nil)
}

/*
Bind values of a new objects to a pointer to struct with option.

ptr: a pointer to struct
opt: attibute values
*/
func (fa *Factory) ConstructWithOption(ptr interface{}, opt map[string]interface{}) error {
	return fa.ConstructWithContextAndOption(context.Background(), ptr, opt)
}

/*
Bind values of a new objects to a pointer to struct with context and option.

ctx: context object
ptr: a pointer to struct
opt: attibute values
*/
func (fa *Factory) ConstructWithContextAndOption(ctx context.Context, ptr interface{}, opt map[string]interface{}) error {
	pt := reflect.TypeOf(ptr)
	if pt.Kind() != reflect.Ptr {
		return errors.New("ptr should be pointer type.")
	}
	pt = pt.Elem()
	if pt.Name() != fa.modelName() {
		return errors.New("ptr type should be " + fa.modelName())
	}

	inst := reflect.ValueOf(ptr).Elem()
	_, err := fa.build(ctx, &inst, pt, opt, nil)
	return err
}

func (fa *Factory) build(ctx context.Context, inst *reflect.Value, tp reflect.Type, opt map[string]interface{}, pl *pipeline) (interface{}, error) {
	args := &argsStruct{}
	args.pl = pl
	args.ctx = ctx
	if fa.isPtr {
		addr := (*inst).Addr()
		args.rv = &addr
	} else {
		args.rv = inst
	}

	for i := 0; i < fa.numField; i++ {
		if v, ok := opt[fa.attrGens[i].key]; ok {
			inst.Field(i).Set(reflect.ValueOf(v))
		} else {
			ag := fa.attrGens[i]
			if ag.genFunc == nil {
				if !ag.isNil {
					inst.Field(i).Set(reflect.ValueOf(ag.value))
				}
			} else {
				v, err := ag.genFunc(args)
				if err != nil {
					return nil, err
				}
				if v != nil {
					inst.Field(i).Set(reflect.ValueOf(v))
				}
			}
		}
	}

	for k, v := range opt {
		setValueWithAttrPath(inst, tp, k, v)
	}

	if fa.onCreate != nil {
		if err := fa.onCreate(args); err != nil {
			return nil, err
		}
	}

	if fa.isPtr {
		return (*inst).Addr().Interface(), nil
	}
	return inst.Interface(), nil
}

func (fa *Factory) create(ctx context.Context, opt map[string]interface{}, pl *pipeline) (interface{}, error) {
	inst := reflect.New(fa.rt).Elem()
	return fa.build(ctx, &inst, fa.rt, opt, pl)
}
