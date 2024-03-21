package compiler

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io/ioutil"
	"os"
	"text/template"

	"github.com/Velocidex/registry_hunter/config"
	"github.com/Velocidex/yaml/v2"
)

const (
	artifact_template = `
name: {{.Name}}
description: |
   This artifact parses and categorizes information for the registry.

   ## Remapping Strategy

   The artifact works by deploying search rules against the registry.
   A rule searches for a specific piece of data

   1. SAM is mapped to /SAM/
   2. NTUser.dat is mapped to HKEY_USERS/*
   3. System and Software hives are mappeed to HKEY_LOCAL_MACHINE and CurrentControlSet

parameters:
- name: CategoryFilter
  type: regex
  default: .
- name: CategoryExcludedFilter
  type: regex
  default: XXXXXX
- name: DescriptionFilter
  type: regex
  default: .
- name: RootFilter
  type: regex
  default: .
- name: RemappingStrategy
  description:
  type: choices
  default: "API And NTUser.dat"
  choices:
   - API
   - API And NTUser.dat
   - Raw Hives

- name: NTFS_CACHE_TIME
  type: int
  description: How often to flush the NTFS cache. (Default is never).
  default: "1000000"


imports:
  - Windows.Registry.NTUser

export: |
    // Map raw hives for hives that are not normally accessible via API
    LET _unmounted_hive_mapping = (
      _map_file_to_reg_path(
          HivePath="C:/Windows/System32/Config/SAM",
          RegistryPath="SAM",
          RegMountPoint="/",
          Accessor="auto",
          Description="Map SAM to /SAM/"),
      _map_file_to_reg_path(
          HivePath="C:/Windows/appcompat/Programs/Amcache.hve",
          RegistryPath="Amcache",
          RegMountPoint="/",
          Accessor="auto",
          Description="Map Amcache to /Amcache/"),
    ) + _required_mappings

    LET _api_remapping <= (dict(
       type="mount",
       ` + "`" + `from` + "`" + `=dict(accessor="registry", prefix='/', path_type='registry'),
       on=dict(accessor="registry", prefix='/', path_type="registry")),
    )

    // The BCD hive is normally located on an unmounted drive so we
    // always map it with the API
    LET _bcd_map <= (dict(
       type="mount",
       ` + "`" + `from` + "`" + `=dict(accessor="registry", prefix='HKEY_LOCAL_MACHINE\\BCD00000000', path_type='registry'),
       on=dict(accessor="registry", prefix='HKEY_LOCAL_MACHINE\\BCD00000000', path_type="registry")))

    // Apply the mappings:
    LET RemapRules = if(condition=RemappingStrategy = "API",
       then=_api_remapping + _unmounted_hive_mapping,
    else=if(condition=RemappingStrategy = "API And NTUser.dat",
       then=_api_remapping + _user_mappings + _unmounted_hive_mapping,
    else=_user_mappings + _unmounted_hive_mapping + _standard_mappings + _bcd_map))

    LET _MD <= parse_json_array(data=gunzip(string=base64decode(string="{{.Metadata }}")))
    LET MD(DescriptionFilter, RootFilter, CategoryFilter, CategoryExcludedFilter) = SELECT * FROM _MD
     WHERE Description =~ DescriptionFilter
       AND Root =~ RootFilter
       AND Category =~ CategoryFilter
       AND NOT Category =~ CategoryExcludedFilter

sources:
- name: Remapping
  query: |
    SELECT * FROM RemapRules

- name: Rules
  query: |
    LET AllRules <=
      SELECT * FROM MD(DescriptionFilter=DescriptionFilter, RootFilter=RootFilter,
        CategoryFilter=CategoryFilter, CategoryExcludedFilter=CategoryExcludedFilter)
    SELECT * FROM AllRules

- name: Globs
  query: |
    LET AllGlobs <=
      SELECT Root, enumerate(items=Glob) AS Globs
      FROM AllRules
      GROUP BY Root

    SELECT * FROM AllGlobs

- query: |
    LET GlobsMD <= to_dict(item={
      SELECT Root AS _key, Globs AS _value FROM AllGlobs
    })

    LET Cache <= memoize(query={
       SELECT Glob, Category, Description FROM AllRules
    }, key="Glob", period=100000)

    LET _ <= remap(config=dict(remappings=RemapRules))

    LET Result = SELECT OSPath, Mtime,
       Data.value AS Data,
       get(item=Cache, field=Globs[0]) AS MetadaData,
       Globs[0] AS _Glob
    FROM foreach(row={
       SELECT _key AS Root, _value AS GlobsToSearch
       FROM items(item=GlobsMD)
       WHERE Root =~ RootFilter
         AND log(message="Will search with globs %v at Root point %v",
             dedup=-1, args=[GlobsToSearch, Root])

    }, query={
       SELECT * FROM glob(globs=GlobsToSearch, root=Root, accessor="registry")
    }, workers=20)

    SELECT MetadaData.Category AS Category, OSPath, Mtime, Data, MetadaData, _Glob
    FROM Result
    WHERE Category =~ CategoryFilter
      AND MetadaData.Description =~ DescriptionFilter
`
)

type templateParameters struct {
	Name     string
	Metadata string
	Rules    []config.RegistryRule
}

type Compiler struct {
	rules []config.RegistryRule
	md    map[string]config.RegistryRule

	// Groups the globs by root and globs
	globs map[string][]string
}

func NewCompiler() *Compiler {
	return &Compiler{
		md:    make(map[string]config.RegistryRule),
		globs: make(map[string][]string),
	}
}

func (self *Compiler) LoadRules(filename string) error {
	fd, err := os.Open(filename)
	if err != nil {
		return err
	}

	data, err := ioutil.ReadAll(fd)
	if err != nil {
		return err
	}

	rules := &config.RuleFile{}
	err = yaml.UnmarshalStrict(data, rules)
	if err != nil {
		return err
	}

	self.rules = append(self.rules, rules.Rules...)
	return nil
}

func (self *Compiler) buildMetadata() string {
	serialized, _ := json.Marshal(self.rules)

	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	gz.Write(serialized)
	gz.Close()
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func (self *Compiler) saveFile(filename string, item interface{}) error {
	out_fd, err := os.OpenFile(filename,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out_fd.Close()

	serialized, err := yaml.Marshal(item)
	if err != nil {
		return err
	}

	_, err = out_fd.Write(serialized)
	return err
}

func (self *Compiler) Compile() (string, error) {
	tmpl, err := template.New("").Parse(artifact_template)
	if err != nil {
		return "", err
	}

	parameters := &templateParameters{
		Name:     "Windows.Registry.Hunter",
		Metadata: self.buildMetadata(),
		Rules:    self.rules,
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, parameters)
	return string(b.Bytes()), err
}