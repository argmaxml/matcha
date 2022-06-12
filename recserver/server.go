package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/DataIntelligenceCrew/go-faiss"
	"github.com/bluele/gcache"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html"
	"gonum.org/v1/gonum/mat"
)

type Schema struct {
	IdCol          string    `json:"id_col"`
	Metric         string    `json:"metric"`
	IndexFactory   string    `json:"index_factory"`
	Filters        []Filter  `json:"filters"`
	Encoders       []Encoder `json:"encoders"`
	Sources        []Source  `json:"sources"`
	Dim            int       `json:"dim"`
	Embeddings     map[string]*mat.Dense
	NumItems       int
	Partitions     [][]string
	PartitionMap   map[string]int
	WeightOverride []WeightOverride `json:"weight_override"`
}

type Filter struct {
	Field   string   `json:"field"`
	Values  []string `json:"values"`
	Default string   `json:"default"`
}

type Encoder struct {
	Field   string   `json:"field"`
	Values  []string `json:"values"`
	Default string   `json:"default"`
	Type    string   `json:"type"`
	Npy     string   `json:"npy"`
	Weight  float64  `json:"weight"`
}

type WeightOverride struct {
	FilterField   string  `json:"filter_field"`
	EncoderField  string  `json:"encoder_field"`
	FilterValue   string  `json:"filter_value"`
	EncoderWeight float64 `json:"encoder_weight"`
}

type Source struct {
	Record string `json:"record"`
	Type   string `json:"type"`
	Path   string `json:"path"`
	Query  string `json:"query"`
}

type Explanation struct {
	Label     string             `json:"label"`
	Distance  float32            `json:"distance"`
	Breakdown map[string]float32 `json:"breakdown"`
}

type QueryRetVal struct {
	Explanations []Explanation `json:"explanations"`
	Variant      string        `json:"variant"`
}

type ItemLookup struct {
	id2label        []string
	label2id        map[string]int
	label2partition map[string]int
}

type Record struct {
	Id        int
	Label     string
	Partition int
	Values    []string
	Fields    []string
}

type PartitionMeta struct {
	Name    []string `json:"name"`
	Count   int      `json:"count"`
	Trained bool     `json:"trained"`
}

type Variant struct {
	Name       string             `json:"name"`
	Percentage float64            `json:"percentage"`
	Weights    map[string]float64 `json:"weights"`
}

func read_index_labels(in_file string) []string {
	jsonFile, err := os.Open(in_file)
	if err != nil {
		fmt.Println(err)
	}
	defer jsonFile.Close()
	byteValue, _ := ioutil.ReadAll(jsonFile)

	var index_labels []string

	json.Unmarshal(byteValue, &index_labels)
	return index_labels
}

func (schema Schema) encode(query map[string]string) []float32 {
	encoded := make([]float64, 0)
	encoder_weights := make([]float64, len(schema.Encoders))
	for i := 0; i < len(schema.Encoders); i++ {
		encoder_weights[i] = schema.Encoders[i].Weight
	}
	// Concatenate all components to a single vector
	for i := 0; i < len(schema.Encoders); i++ {
		var raw_vector []float64
		encoder_type := strings.ToLower(schema.Encoders[i].Type)
		val, found := query[schema.Encoders[i].Field]
		if !found {
			val = schema.Encoders[i].Default
		}
		// Override weight if specified
		for j := 0; j < len(schema.Filters); j++ {
			for _, weight_override := range schema.WeightOverride {
				if weight_override.EncoderField == schema.Encoders[i].Field && weight_override.FilterField == schema.Filters[j].Field && weight_override.FilterValue == query[schema.Filters[j].Field] {
					encoder_weights[i] = weight_override.EncoderWeight
				}
			}
		}
		if contains([]string{"numeric", "num", "scalar"}, encoder_type) {
			fval, err := strconv.ParseFloat(val, 64)
			if err != nil {
				fval = 0
			}
			raw_vector = []float64{fval * encoder_weights[i]}
		} else {
			emb_matrix := schema.Embeddings[schema.Encoders[i].Field]
			row_index := index_of(schema.Encoders[i].Values, val)
			if row_index == -1 { // not found, use default
				row_index = index_of(schema.Encoders[i].Values, schema.Encoders[i].Default)
			}
			_, emb_size := emb_matrix.Dims()
			raw_vector = make([]float64, emb_size)
			if row_index > -1 {
				raw_vector = mat.Row(nil, row_index, emb_matrix)
				for j := 0; j < emb_size; j++ {
					raw_vector[j] *= encoder_weights[i]
				}
			}
		}
		encoded = append(encoded, raw_vector...)
	}
	// Convert to float32
	encoded32 := make([]float32, len(encoded))
	for i, f64 := range encoded {
		encoded32[i] = float32(f64)
	}

	return encoded32
}

func (schema Schema) partition_number(query map[string]string, variant string) int {

	filters := make([]string, len(schema.Filters))
	for i := 1; i < len(schema.Filters); i++ {
		val, found := query[schema.Filters[i].Field]
		if !found {
			val = schema.Filters[i].Default
		}
		filters[i] = val
	}
	filters[0] = variant
	partition_key := strings.Join(filters, "~")
	partition_idx := schema.PartitionMap[partition_key]
	return partition_idx
}

func (schema Schema) componentwise_distance(v1 []float32, v2 []float32) (float32, map[string]float32) {
	breakdown := make(map[string]float32)
	var total_distance float32
	total_distance = 0
	offset := 0
	for _, encoder := range schema.Encoders {
		if contains([]string{"np", "numpy", "npy"}, strings.ToLower(encoder.Type)) {
			emb_matrix := schema.Embeddings[encoder.Field]
			_, emb_size := emb_matrix.Dims()
			breakdown[encoder.Field] = 0
			for i := 0; i < emb_size; i++ {
				if strings.ToLower(schema.Metric) == "l1" {
					if v1[offset+i] > v2[offset+i] {
						breakdown[encoder.Field] += (v1[offset+i] - v2[offset+i])
					} else {
						breakdown[encoder.Field] += (v2[offset+i] - v1[offset+i])
					}
				}
				if strings.ToLower(schema.Metric) == "l2" {
					breakdown[encoder.Field] += (v1[offset+i] - v2[offset+i]) * (v1[offset+i] - v2[offset+i])
				}
				//TODO: Support InnerProduct
				total_distance += breakdown[encoder.Field]
			}
			if strings.ToLower(schema.Metric) == "l2" {
				breakdown[encoder.Field] = float32(math.Sqrt(float64(breakdown[encoder.Field])))
			}
			breakdown[encoder.Field] /= float32(emb_size)
			offset += emb_size
		} else { //numeric field
			if v1[offset] > v2[offset] {
				breakdown[encoder.Field] += (v1[offset] - v2[offset])
			} else {
				breakdown[encoder.Field] += (v2[offset] - v1[offset])
			}
			offset += 1
		}

	}
	if strings.ToLower(schema.Metric) == "l2" {
		total_distance = float32(math.Sqrt(float64(total_distance)))
	}
	total_distance /= float32(schema.Dim)
	return total_distance, breakdown
}

func (schema Schema) reconstruct(partitioned_records map[int][]Record, id int64, partition_idx int) []float32 {
	var reconstructed []float32
	reconstructed = nil
	//TODO: Have a more intelligent way of looking up the original record (currently, linear search)
	for _, record := range partitioned_records[partition_idx] {
		if record.Id == int(id) {
			reconstructed = schema.encode(zip(record.Fields, record.Values))
			break
		}
	}
	return reconstructed
}

func faiss_index_from_cache(cache gcache.Cache, index int) faiss.Index {
	faiss_interface, _ := cache.Get(index)
	return faiss_interface.(faiss.Index)
}

func random_variant(variants []Variant) string {
	weights := make([]float64, len(variants))
	names := make([]string, len(variants))
	for i := 0; i < len(variants); i++ {
		weights[i] = variants[i].Percentage
		names[i] = variants[i].Name
	}
	retval := random_by_weights(names, weights)
	if retval == "default" {
		return ""
	}
	return retval
}

func start_server(schema Schema, variants []Variant, indices gcache.Cache, item_lookup ItemLookup, partitioned_records map[int][]Record, user_data map[string][]string) {
	app := fiber.New(fiber.Config{
		Views: html.New("./views", ".html"),
	})

	var faiss_index faiss.Index
	// GET /api/register
	app.Get("/npy/*", func(c *fiber.Ctx) error {
		m := read_npy(c.Params("*") + ".npy")
		msg := fmt.Sprintf("data = %v\n", mat.Formatted(m, mat.Prefix("       ")))
		return c.SendString(msg)
	})

	app.Get("/partitions", func(c *fiber.Ctx) error {
		ret := make([]PartitionMeta, len(schema.Partitions))
		for i, partition := range schema.Partitions {
			ret[i].Name = partition
			//TODO: fix
			// ret[i].Count = int(indices[i].Ntotal())
			// ret[i].Trained = indices[i].IsTrained()
		}
		return c.JSON(ret)
	})

	app.Get("/labels", func(c *fiber.Ctx) error {
		return c.JSON(item_lookup.id2label)
	})

	app.Get("/reload_items", func(c *fiber.Ctx) error {
		partitioned_records, item_lookup, _ = schema.pull_item_data(variants)
		os.RemoveAll("indices")
		schema.index_partitions(partitioned_records)
		return c.SendString("{\"Status\": \"OK\"}")
	})

	app.Get("/reload_users", func(c *fiber.Ctx) error {
		var err error
		if user_data == nil {
			return c.SendString("User history not available in sources list")
		}
		user_data, err = schema.pull_user_data()
		if err != nil {
			return c.SendString(err.Error())
		}
		return c.SendString("{\"Status\": \"OK\"}")
	})

	app.Post("/encode", func(c *fiber.Ctx) error {
		var query map[string]string
		json.Unmarshal(c.Body(), &query)
		encoded := schema.encode(query)
		return c.JSON(encoded)
	})

	app.Post("/item_query/:k?", func(c *fiber.Ctx) error {
		payload := struct {
			ItemId  string            `json:"id"`
			Query   map[string]string `json:"query"`
			Explain bool              `json:"explain"`
		}{}

		if err := c.BodyParser(&payload); err != nil {
			return err
		}
		k, err := strconv.Atoi(c.Params("k"))
		if err != nil {
			k = 2
		}
		var partition_idx int
		var encoded []float32
		variant := random_variant(variants)
		if payload.ItemId != "" {
			id := int64(item_lookup.label2id[variant+"~"+payload.ItemId])
			partition_idx = item_lookup.label2partition[variant+"~"+payload.ItemId]
			encoded = schema.reconstruct(partitioned_records, id, partition_idx)
			if encoded == nil {
				return c.SendString("{\"Status\": \"Not Found\"}")
			}
		} else {
			partition_idx = schema.partition_number(payload.Query, variant)
			encoded = schema.encode(payload.Query)
		}
		//TODO: Resolve code duplication (1)
		faiss_index = faiss_index_from_cache(indices, partition_idx)
		distances, ids, err := faiss_index.Search(encoded, int64(k))
		if err != nil {
			log.Fatal(err)
		}
		retrieved := make([]Explanation, 0)
		for i, id := range ids {
			if id == -1 {
				continue
			}
			next_result := Explanation{
				Label:    strings.SplitN(item_lookup.id2label[int(id)], "~", 2)[1],
				Distance: distances[i],
			}
			if (payload.Explain) && (partitioned_records != nil) {
				reconstructed := schema.reconstruct(partitioned_records, id, partition_idx)
				if reconstructed != nil {
					total_distance, breakdown := schema.componentwise_distance(encoded, reconstructed)
					next_result.Distance = total_distance
					next_result.Breakdown = breakdown
				}
			}
			retrieved = append(retrieved, next_result)
		}
		if variant == "" {
			variant = "default"
		}
		retval := QueryRetVal{
			Explanations: retrieved,
			Variant:      variant,
		}
		return c.JSON(retval)
	})

	app.Post("/user_query/:k?", func(c *fiber.Ctx) error {
		payload := struct {
			UserId  string            `json:"id"`
			History []string          `json:"history"`
			Filters map[string]string `json:"filters"`
			Explain bool              `json:"explain"`
		}{}

		if err := c.BodyParser(&payload); err != nil {
			return err
		}
		variant := random_variant(variants)
		partition_idx := schema.partition_number(payload.Filters, variant)
		k, err := strconv.Atoi(c.Params("k"))
		if err != nil {
			k = 2
		}
		item_vecs := make([][]float32, 1)
		item_vecs[0] = make([]float32, schema.Dim) // zero_vector

		if payload.UserId != "" {
			//Override user history from the id, if provided
			if user_data == nil {
				return c.SendString("User history not available in sources list")
			}
			payload.History = user_data[payload.UserId]
		}

		for _, item_id := range payload.History {
			id := int64(item_lookup.label2id[variant+"~"+item_id])
			if id == -1 {
				continue
			}
			reconstructed := schema.reconstruct(partitioned_records, id, partition_idx)
			if reconstructed == nil {
				continue
			}
			item_vecs = append(item_vecs, reconstructed)
		}
		//TODO: Account for cold start
		user_vec := make([]float32, schema.Dim)
		for _, item_vec := range item_vecs {
			for i := range user_vec {
				user_vec[i] += item_vec[i] / float32(len(item_vecs))
			}
		}

		//TODO: Resolve code duplication (2)
		faiss_index = faiss_index_from_cache(indices, partition_idx)
		distances, ids, err := faiss_index.Search(user_vec, int64(k))
		if err != nil {
			log.Fatal(err)
		}
		retrieved := make([]Explanation, 0)
		for i, id := range ids {
			if id == -1 {
				continue
			}
			next_result := Explanation{
				Label:    strings.SplitN(item_lookup.id2label[int(id)], "~", 2)[1],
				Distance: distances[i],
			}
			if (payload.Explain) && (partitioned_records != nil) {
				reconstructed := schema.reconstruct(partitioned_records, id, partition_idx)
				if reconstructed != nil {
					total_distance, breakdown := schema.componentwise_distance(user_vec, reconstructed)
					next_result.Distance = total_distance
					next_result.Breakdown = breakdown
				}
			}
			retrieved = append(retrieved, next_result)
		}
		if variant == "" {
			variant = "default"
		}
		retval := QueryRetVal{
			Explanations: retrieved,
			Variant:      variant,
		}
		return c.JSON(retval)
	})

	app.Get("/", func(c *fiber.Ctx) error {
		fields := make([]string, 0)
		filters := make([]string, 0)
		for _, e := range schema.Filters {
			fields = append(fields, e.Field)
			filters = append(filters, e.Field)
		}
		for _, e := range schema.Encoders {
			fields = append(fields, e.Field)
		}
		return c.Render("index", fiber.Map{
			"Headline": "Recsplain API",
			"Fields":   fields,
			"Filters":  filters,
		})
	})

	log.Fatal(app.Listen(":8088"))
}

func (schema Schema) read_partitioned_csv(filename string, variants []Variant) (map[int][]Record, ItemLookup, error) {
	header, data, err := read_csv(filename)
	if err != nil {
		return nil, ItemLookup{}, err
	}
	id_num := index_of(header, schema.IdCol)
	if id_num == -1 {
		return nil, ItemLookup{}, errors.New("id column not found")
	}

	label2id := make(map[string]int)
	label2partition := make(map[string]int)
	partition2records := make(map[int][]Record)
	for _, variant := range variants {
		for _, row := range data {
			vid := variant.Name + "~" + row[id_num]
			id, found := label2id[vid]
			if !found {
				id = len(label2id)
				label2id[vid] = id
			}
			query := zip(header, row)

			partition_idx := schema.partition_number(query, variant.Name)
			label2partition[vid] = partition_idx
			partition2records[partition_idx] = append(partition2records[partition_idx], Record{
				Label:     row[id_num],
				Id:        id,
				Values:    row,
				Fields:    header,
				Partition: partition_idx,
			})
		}

	}
	id2label := make([]string, len(label2id))
	for lbl, id := range label2id {
		id2label[id] = lbl
	}
	// Build lookups
	item_lookup := ItemLookup{
		label2id:        label2id,
		id2label:        id2label,
		label2partition: label2partition,
	}
	item_lookup.id2label = id2label
	return partition2records, item_lookup, nil
}

func (schema Schema) index_partitions(records map[int][]Record) {
	os.Mkdir("indices", os.ModePerm)
	var wg sync.WaitGroup
	for partition_idx, partitioned_records := range records {
		if len(partitioned_records) < 10 {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("indices/%d", partition_idx)); !os.IsNotExist(err) {
			continue
		}

		wg.Add(1)
		go func(partition_idx int, partitioned_records []Record) {
			defer wg.Done()
			var faiss_index faiss.Index
			// https://github.com/facebookresearch/faiss/wiki/The-index-factory
			index_factory := schema.IndexFactory
			if schema.IndexFactory == "" { //auto compute
				n_clusters := 128
				if len(partitioned_records) < n_clusters {
					n_clusters = len(partitioned_records)
				}
				index_factory = fmt.Sprintf("IVF%d,Flat", n_clusters)
			}
			if strings.ToLower(schema.Metric) == "ip" {
				faiss_index, _ = faiss.IndexFactory(schema.Dim, index_factory, faiss.MetricInnerProduct)
			}
			if strings.ToLower(schema.Metric) == "l2" {
				faiss_index, _ = faiss.IndexFactory(schema.Dim, index_factory, faiss.MetricL2)
			}
			if strings.ToLower(schema.Metric) == "l1" {
				faiss_index, _ = faiss.IndexFactory(schema.Dim, index_factory, faiss.MetricL1)
			}
			xb := make([]float32, schema.Dim*len(partitioned_records))
			ids := make([]int64, len(partitioned_records))
			for i, record := range partitioned_records {
				encoded := schema.encode(zip(record.Fields, record.Values))
				for j, v := range encoded {
					xb[i*schema.Dim+j] = v
					ids[i] = int64(record.Id)
				}
			}
			// fmt.Printf("Start-%d\n", partition_idx)
			faiss_index.Train(xb)
			faiss_index.AddWithIDs(xb, ids)
			faiss_index.Train(xb)
			faiss.WriteIndex(faiss_index, fmt.Sprintf("indices/%d", partition_idx))
			faiss_index.Delete()
			// fmt.Printf("Done-%d\n", partition_idx)

		}(partition_idx, partitioned_records)
	}
	wg.Wait()
}

func (schema Schema) pull_item_data(variants []Variant) (map[int][]Record, ItemLookup, error) {
	var item_lookup ItemLookup
	var partitioned_records map[int][]Record
	var err error
	found_item_source := false
	for _, src := range schema.Sources {
		if strings.ToLower(src.Record) == "items" {
			if src.Type == "csv" {
				partitioned_records, item_lookup, err = schema.read_partitioned_csv(src.Path, variants)
				if err != nil {
					return nil, ItemLookup{}, err
				}
				found_item_source = true
			}
		}
	}
	if !found_item_source {
		return nil, ItemLookup{}, errors.New("no item source found")
	}
	return partitioned_records, item_lookup, err
}

func (schema Schema) pull_user_data() (map[string][]string, error) {
	var user_data map[string][]string
	var err error
	found_user_source := false
	for _, src := range schema.Sources {
		if strings.ToLower(src.Record) == "users" {
			if src.Type == "csv" {
				user_data, err = schema.read_user_csv(src.Path, src.Query)
				if err != nil {
					return nil, err
				}
				found_user_source = true
			}
		}
	}
	if !found_user_source {
		return nil, errors.New("no user source found")
	}
	return user_data, err
}

func (schema Schema) read_user_csv(filename string, history_col string) (map[string][]string, error) {

	header, data, err := read_csv(filename)
	if err != nil {
		fmt.Println(err.Error())
	}
	id_num := index_of(header, schema.IdCol)
	if id_num == -1 {
		return nil, errors.New("id column not found")
	}

	history_num := index_of(header, history_col)
	if id_num == -1 {
		return nil, errors.New("history column not found")
	}

	user_data := make(map[string][]string)
	for _, row := range data {
		user_id := row[id_num]
		user_data[user_id] = strings.Split(row[history_num], ",")
	}

	return user_data, nil
}

func read_schema(schema_file string, variants_file string) (Schema, []Variant, error) {
	schema_json_file, err := os.Open(schema_file)
	if err != nil {
		fmt.Println(err)
		return Schema{}, nil, err
	}
	defer schema_json_file.Close()
	schema_byte_value, _ := ioutil.ReadAll(schema_json_file)
	var schema Schema
	json.Unmarshal(schema_byte_value, &schema)

	variants_json_file, err := os.Open(variants_file)
	var variants []Variant
	if err == nil {
		defer variants_json_file.Close()
		variants_byte_value, _ := ioutil.ReadAll(variants_json_file)
		json.Unmarshal(variants_byte_value, &variants)
	} else {
		fmt.Println("Variant file not found, using default 100 percent split")
		variants := make([]Variant, 1)
		variants[0] = Variant{
			Name:       "",
			Percentage: 100,
			Weights:    make(map[string]float64),
		}
	}

	if schema.WeightOverride == nil {
		schema.WeightOverride = make([]WeightOverride, 0)
	}
	variants_vals := make([]string, len(variants))
	for i, variant := range variants {
		variants_vals[i] = variant.Name
	}
	variant_filter := make([]Filter, 1)
	variant_filter[0] = Filter{
		Field:   "variant",
		Default: "",
		Values:  variants_vals,
	}
	schema.Filters = append(variant_filter, schema.Filters...)

	embeddings := make(map[string]*mat.Dense)
	dim := 0
	for i := 0; i < len(schema.Encoders); i++ {
		encoder_type := strings.ToLower(schema.Encoders[i].Type)
		if contains([]string{"np", "numpy", "npy"}, encoder_type) {
			embeddings[schema.Encoders[i].Field] = read_npy(schema.Encoders[i].Npy)
			_, emb_size := embeddings[schema.Encoders[i].Field].Dims()
			dim += emb_size
		}
		if contains([]string{"numeric", "num", "scalar"}, encoder_type) {
			dim += 1
		}
	}
	schema.Dim = dim
	schema.Embeddings = embeddings

	//Add weight overloading
	varianted_weights := make([]WeightOverride, 0)
	for _, variant := range variants {
		for encoder_field, encoder_weight := range variant.Weights {
			varianted_weights = append(varianted_weights, WeightOverride{
				FilterField:   "variant",
				FilterValue:   variant.Name,
				EncoderField:  encoder_field,
				EncoderWeight: encoder_weight,
			})
		}
	}
	schema.WeightOverride = append(schema.WeightOverride, varianted_weights...)

	values := make([][]string, len(schema.Filters))
	for i := 0; i < len(schema.Filters); i++ {
		values[i] = schema.Filters[i].Values
	}
	partitions := itertools_product(values...)

	schema.Partitions = partitions

	partition_map := make(map[string]int)
	for i := 0; i < len(partitions); i++ {
		key := strings.Join(partitions[i], "~")
		partition_map[key] = i
	}
	schema.PartitionMap = partition_map
	return schema, variants, nil
}

func main() {
	base_dir := "."
	if len(os.Args) > 1 {
		base_dir = os.Args[1]
	}

	schema, variants, err := read_schema(base_dir+"/schema.json", base_dir+"/variants.json")
	if err != nil {
		fmt.Println(err)
	}

	indices := gcache.New(32).
		LFU().
		LoaderFunc(func(key interface{}) (interface{}, error) {
			ind, err := faiss.ReadIndex(fmt.Sprintf("%s/indices/%d", base_dir, key), 0)
			return *ind, err
		}).
		EvictedFunc(func(key, value interface{}) {
			value.(faiss.Index).Delete()
		}).
		Build()

	// indices := make([]faiss.IndexImpl, len(partitions))
	// indices := make([]faiss.Index, len(partitions))

	var partitioned_records map[int][]Record
	var user_data map[string][]string

	//TODO: Read from CLI
	item_lookup := ItemLookup{
		id2label:        make([]string, 0),
		label2id:        make(map[string]int),
		label2partition: make(map[string]int),
	}
	partitioned_records, item_lookup, err = schema.pull_item_data(variants)
	if err != nil {
		log.Fatal(err)
	}
	user_data, err = schema.pull_user_data()
	if err != nil {
		log.Println(err)
	}

	schema.index_partitions(partitioned_records)

	start_server(schema, variants, indices, item_lookup, partitioned_records, user_data)
}
