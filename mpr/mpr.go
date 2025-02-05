package mpr

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"go.mongodb.org/mongo-driver/bson"

	_ "github.com/glebarez/go-sqlite"
)

func ExportModel(inputDirectory string, outputDirectory string, raw bool, mode string) error {
	err := filepath.Walk(inputDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, ".mendix-cache") {
			log.Debugf("Skipping system managed file %s", path)
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".mpr") {
			exportMPR(path, outputDirectory, raw, mode)
		}
		return nil
	})
	return err
}

func exportMetadata(MPRFilePath string, outputDirectory string) error {

	db, err := sql.Open("sqlite", MPRFilePath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query("SELECT _ProductVersion, _BuildVersion FROM _MetaData")
	if err != nil {
		return fmt.Errorf("error querying units: %v", err)
	}

	log.Debugf("Exporting metadata")
	defer rows.Close()

	if !rows.Next() {
		return fmt.Errorf("no metadata found")
	}

	var productVersion, buildVersion string
	if err := rows.Scan(&productVersion, &buildVersion); err != nil {
		return fmt.Errorf("error scanning metadata: %v", err)
	}

	units, err := getMxUnits(MPRFilePath)
	if err != nil {
		return fmt.Errorf("error getting units: %v", err)
	}
	modules := getMxModules(units)

	// create metadata object
	metadataObj := MxMetadata{
		ProductVersion: productVersion,
		BuildVersion:   buildVersion,
		Modules:        modules,
	}

	// write metadata to file
	metadataYAML, err := yaml.Marshal(metadataObj)
	if err != nil {
		return fmt.Errorf("error marshaling metadata: %v", err)
	}

	if _, err := os.Stat(outputDirectory); os.IsNotExist(err) {
		if err := os.MkdirAll(outputDirectory, 0755); err != nil {
			return fmt.Errorf("error creating directory: %v", err)
		}
	}
	metadataFileName := filepath.Join(outputDirectory, "Metadata.yaml")

	if err := os.WriteFile(metadataFileName, metadataYAML, 0644); err != nil {
		return fmt.Errorf("error writing metadata file: %v", err)
	}

	return nil

}

func getMxModules(units []MxUnit) []MxModule {
	modules := make([]MxModule, 0)
	for _, unit := range units {
		if unit.ContainmentName == "Modules" {
			myModule := MxModule{
				Name:       unit.Contents["Name"].(string),
				ID:         unit.UnitID,
				Attributes: unit.Contents,
			}
			modules = append(modules, myModule)
		}
	}
	return modules
}

func getMxFolders(units []MxUnit) ([]MxFolder, error) {
	var folders []MxFolder
	for _, unit := range units {
		if unit.ContainmentName == "Folders" || unit.ContainmentName == "Modules" {
			log.Debugf("Unit: %v", unit)
			myFolder := MxFolder{
				Name:       unit.Contents["Name"].(string),
				ID:         unit.UnitID,
				ParentID:   unit.ContainerID,
				Attributes: unit.Contents,
				Parent:     nil,
			}
			folders = append(folders, myFolder)
		} else if unit.ContainmentName == "" {
			myFolder := MxFolder{
				Name:       ".",
				ID:         unit.UnitID,
				ParentID:   unit.ContainerID,
				Attributes: unit.Contents,
				Parent:     nil,
			}
			folders = append(folders, myFolder)
		}
	}

	// Temporary map to hold folder references for easy lookup.
	folderMap := make(map[string]*MxFolder)
	for i := range folders {
		folderMap[folders[i].ID] = &folders[i]
	}

	// Set up the parent references.
	for i, folder := range folders {
		if parent, exists := folderMap[folder.ParentID]; exists && folder.ParentID != folder.ID {
			folders[i].Parent = parent
		}
	}

	return folders, nil
}

func getMxDocumentPathRecursive(folder MxFolder, depth int) string {
	if depth == 0 {
		return ""
	}
	if folder.Parent == nil {
		return folder.Name
	} else {
		return filepath.Join(getMxDocumentPathRecursive(*folder.Parent, depth-1), folder.Name)
	}
}

func getMxDocumentPath(containerID string, folders []MxFolder) string {
	for _, folder := range folders {
		if folder.ID == containerID {
			return getMxDocumentPathRecursive(folder, 10)
		}
	}
	return ""
}

func getMxDocuments(units []MxUnit, folders []MxFolder, mode string) ([]MxDocument, error) {
	var documents []MxDocument
	documentTypes := []string{"ProjectDocuments", "DomainModel", "ModuleSettings", "ModuleSecurity", "Documents"}

	for _, unit := range units {
		if Contains(documentTypes, unit.ContainmentName) {
			log.Debugf("Unit: %v", unit)
			var name = ""
			if unit.Contents["Name"] != nil {
				name = unit.Contents["Name"].(string)
			}

			myDocument := MxDocument{
				Name:       name,
				Type:       unit.Contents["$Type"].(string),
				Path:       getMxDocumentPath(unit.ContainerID, folders),
				Attributes: unit.Contents,
			}

			if mode == "advanced" && unit.Contents["$Type"] == "Microflows$Microflow" {
				myDocument = transformMicroflow(myDocument)
			}
			documents = append(documents, myDocument)
		}
	}
	log.Infof("Found %d documents", len(documents))
	return documents, nil
}

func getMxUnits(MPRFilePath string) ([]MxUnit, error) {
	db, err := sql.Open("sqlite", MPRFilePath)
	if err != nil {
		return nil, fmt.Errorf("error opening database: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT UnitID, ContainerID, ContainmentName, Contents FROM Unit")
	if err != nil {
		return nil, fmt.Errorf("error querying units: %v", err)
	}
	defer rows.Close()

	units := make([]MxUnit, 0)

	for rows.Next() {
		var containmentName string
		var unitID, containerID, contents []byte
		if err := rows.Scan(&unitID, &containerID, &containmentName, &contents); err != nil {
			return nil, fmt.Errorf("error scanning unit: %v", err)
		}

		var result bson.M

		err := bson.Unmarshal(contents, &result)
		if err != nil {
			return nil, fmt.Errorf("error parsing unit: %v", err)
		}

		// create unit object
		myUnit := MxUnit{
			UnitID:          base64.StdEncoding.EncodeToString(unitID),
			ContainerID:     base64.StdEncoding.EncodeToString(containerID),
			ContainmentName: containmentName,
			Contents:        result,
		}

		units = append(units, myUnit)
	}
	return units, nil
}

func exportUnits(MPRFilePath string, outputDirectory string, raw bool, mode string) error {

	units, err := getMxUnits(MPRFilePath)
	if err != nil {
		return fmt.Errorf("error getting units: %v", err)
	}
	folders, err := getMxFolders(units)
	if err != nil {
		return fmt.Errorf("error getting folders: %v", err)
	}
	documents, err := getMxDocuments(units, folders, mode)
	if err != nil {
		return fmt.Errorf("error getting documents: %v", err)
	}
	for _, document := range documents {
		// write document
		directory := filepath.Join(outputDirectory, document.Path)
		// ensure directory exists
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			if err := os.MkdirAll(directory, 0755); err != nil {
				return fmt.Errorf("error creating directory: %v", err)
			}
		}
		fname := fmt.Sprintf("%s.%s.yaml", document.Name, document.Type)
		if document.Name == "" {
			fname = fmt.Sprintf("%s.yaml", document.Type)
		}
		attributes := cleanData(document.Attributes, raw)
		err = writeFile(filepath.Join(directory, fname), attributes)
		if err != nil {
			log.Errorf("Error writing file: %v", err)
			return err
		}
	}

	return nil

}

func writeFile(filepath string, contents map[string]interface{}) error {
	log.Debugf("Writing file %s", filepath)
	yamlstring, err := yaml.Marshal(contents)
	if err != nil {
		return fmt.Errorf("error marshaling: %v", err)
	}

	if err := os.WriteFile(filepath, yamlstring, 0644); err != nil {
		return fmt.Errorf("error writing file: %v", err)
	}
	return nil
}

func exportMPR(MPRFilePath string, outputDirectory string, raw bool, mode string) error {
	log.Infof("Exporting %s to %s", MPRFilePath, outputDirectory)
	if err := exportMetadata(MPRFilePath, outputDirectory); err != nil {
		return fmt.Errorf("error exporting metadata: %v", err)
	}

	if err := exportUnits(MPRFilePath, outputDirectory, raw, mode); err != nil {
		return fmt.Errorf("error exporting units: %v", err)
	}
	log.Infof("Completed %s", MPRFilePath)
	return nil
}
