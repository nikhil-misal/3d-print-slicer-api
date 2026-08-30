from fastapi import FastAPI, UploadFile, File, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os
import numpy as np


# ==========================================
# FASTAPI APP
# ==========================================

app = FastAPI(
    title="3D Print Weight Calculator API",
    description="STL model volume, dimensions and estimated 3D print weight calculator",
    version="2.1.0"
)


# ==========================================
# CORS SETTINGS
# ==========================================

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


# ==========================================
# MATERIAL DENSITIES
# Unit: g/cm³
# ==========================================

MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.24,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


# ==========================================
# STANDARD PRINT SETTINGS
# ==========================================

# Standard layer height
LAYER_HEIGHT_MM = 0.20

# Approximate total wall thickness
# Suitable for a typical 0.4 mm nozzle with 2 walls
WALL_THICKNESS_MM = 0.80

# Approximate top and bottom thickness
TOP_BOTTOM_THICKNESS_MM = 0.80

# Correction factor calibrated for slicer-like estimation
PRINT_CORRECTION_FACTOR = 1.028


# ==========================================
# HOME ROUTE
# ==========================================

@app.get("/")
def home():
    return {
        "status": "online",
        "message": "3D Print Weight Calculator API is running",
        "version": "2.1.0"
    }


# ==========================================
# HEALTH CHECK
# ==========================================

@app.get("/health")
def health_check():
    return {
        "success": True,
        "status": "healthy"
    }


# ==========================================
# CALCULATE STL SURFACE AREA
# ==========================================

def calculate_surface_area(vectors):
    """
    Calculate total STL surface area.

    STL vectors shape:
    (number_of_triangles, 3_vertices, 3_coordinates)
    """

    # Triangle vertices
    v0 = vectors[:, 0, :]
    v1 = vectors[:, 1, :]
    v2 = vectors[:, 2, :]

    # Triangle edges
    edge1 = v1 - v0
    edge2 = v2 - v0

    # Cross product
    cross_product = np.cross(edge1, edge2)

    # Area of each triangle
    triangle_areas = (
        np.linalg.norm(cross_product, axis=1) / 2.0
    )

    # Total surface area
    total_area = np.sum(triangle_areas)

    return abs(float(total_area))


# ==========================================
# ANALYZE STL
# ==========================================

@app.post("/analyze")
async def analyze_stl(

    file: UploadFile = File(...),

    material: str = "PLA",

    infill: float = 20

):

    # ------------------------------------------
    # 1. VALIDATE FILE
    # ------------------------------------------

    if not file.filename:
        raise HTTPException(
            status_code=400,
            detail="No file name received"
        )

    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )


    # ------------------------------------------
    # 2. VALIDATE MATERIAL
    # ------------------------------------------

    material_key = material.upper().strip()

    if material_key not in MATERIAL_DENSITIES:

        raise HTTPException(
            status_code=400,
            detail=(
                f"Unsupported material: {material}. "
                f"Supported materials: "
                f"{list(MATERIAL_DENSITIES.keys())}"
            )
        )


    # ------------------------------------------
    # 3. VALIDATE INFILL
    # ------------------------------------------

    try:
        infill = float(infill)
    except ValueError:

        raise HTTPException(
            status_code=400,
            detail="Infill must be a valid number"
        )

    if infill < 0 or infill > 100:

        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )


    # ------------------------------------------
    # TEMPORARY FILE PATH
    # ------------------------------------------

    temp_path = None


    try:

        # --------------------------------------
        # 4. SAVE UPLOADED FILE TEMPORARILY
        # --------------------------------------

        content = await file.read()

        if not content:

            raise HTTPException(
                status_code=400,
                detail="Uploaded STL file is empty"
            )


        with tempfile.NamedTemporaryFile(
            delete=False,
            suffix=".stl"
        ) as temp_file:

            temp_path = temp_file.name

            temp_file.write(content)


        # --------------------------------------
        # 5. LOAD STL MODEL
        # --------------------------------------

        model = mesh.Mesh.from_file(temp_path)


        if len(model.vectors) == 0:

            raise HTTPException(
                status_code=400,
                detail="STL model contains no valid triangles"
            )


        # ======================================
        # 6. STANDARD UNIT
        # ======================================
        #
        # STL normally does not contain unit
        # information.
        #
        # Standard 3D printing assumption:
        # 1 STL coordinate = 1 millimeter
        #
        # All calculations are processed in mm.
        # ======================================

        processing_unit = "mm"


        # --------------------------------------
        # 7. CALCULATE MODEL VOLUME
        # --------------------------------------

        volume_mm3, _, _ = model.get_mass_properties()

        volume_mm3 = abs(float(volume_mm3))

        if not np.isfinite(volume_mm3):

            raise HTTPException(
                status_code=400,
                detail="Could not calculate valid model volume"
            )


        if volume_mm3 <= 0:

            raise HTTPException(
                status_code=400,
                detail=(
                    "Model volume is zero or invalid. "
                    "The STL may be open, broken or non-manifold."
                )
            )


        # Convert mm³ to cm³
        volume_cm3 = volume_mm3 / 1000.0


        # --------------------------------------
        # 8. CALCULATE DIMENSIONS
        # --------------------------------------

        min_values = model.vectors.min(axis=(0, 1))
        max_values = model.vectors.max(axis=(0, 1))

        dimensions_mm = max_values - min_values

        x_mm = float(dimensions_mm[0])
        y_mm = float(dimensions_mm[1])
        z_mm = float(dimensions_mm[2])

        max_dimension_mm = max(
            x_mm,
            y_mm,
            z_mm
        )


        # --------------------------------------
        # 9. CALCULATE SURFACE AREA
        # --------------------------------------

        surface_area_mm2 = calculate_surface_area(
            model.vectors
        )


        if surface_area_mm2 <= 0:

            raise HTTPException(
                status_code=400,
                detail="Could not calculate valid surface area"
            )


        # --------------------------------------
        # 10. GET MATERIAL DENSITY
        # --------------------------------------

        density = MATERIAL_DENSITIES[
            material_key
        ]


        # --------------------------------------
        # 11. CALCULATE SOLID WEIGHT
        # --------------------------------------

        solid_weight_g = (
            volume_cm3 * density
        )


        # ======================================
        # 12. CALCULATE SHELL MATERIAL VOLUME
        # ======================================
        #
        # Approximation:
        # Surface Area × Wall Thickness
        # ======================================

        shell_volume_mm3 = (
            surface_area_mm2 *
            WALL_THICKNESS_MM
        )


        # Shell can never use more than 90%
        # of the total model volume.
        shell_volume_mm3 = min(
            shell_volume_mm3,
            volume_mm3 * 0.90
        )


        # --------------------------------------
        # 13. CALCULATE INTERNAL EMPTY VOLUME
        # --------------------------------------

        internal_volume_mm3 = max(
            0.0,
            volume_mm3 - shell_volume_mm3
        )


        # --------------------------------------
        # 14. CALCULATE INFILL MATERIAL VOLUME
        # --------------------------------------

        infill_ratio = infill / 100.0

        infill_volume_mm3 = (
            internal_volume_mm3 *
            infill_ratio
        )


        # --------------------------------------
        # 15. TOTAL ESTIMATED MATERIAL VOLUME
        # --------------------------------------

        estimated_material_volume_mm3 = (
            shell_volume_mm3 +
            infill_volume_mm3
        )


        # Convert mm³ to cm³
        estimated_material_volume_cm3 = (
            estimated_material_volume_mm3 /
            1000.0
        )


        # --------------------------------------
        # 16. ESTIMATED PRINT WEIGHT
        # --------------------------------------

        estimated_print_weight_g = (
            estimated_material_volume_cm3 *
            density *
            PRINT_CORRECTION_FACTOR
        )


        # --------------------------------------
        # 17. MAXIMUM ALLOWED WEIGHT
        # --------------------------------------

        maximum_weight_g = (
            solid_weight_g *
            PRINT_CORRECTION_FACTOR
        )


        # Weight should not exceed solid model
        estimated_print_weight_g = min(
            estimated_print_weight_g,
            maximum_weight_g
        )


        # --------------------------------------
        # 18. MATERIAL USAGE PERCENTAGE
        # --------------------------------------

        if maximum_weight_g > 0:

            material_usage_percent = (
                estimated_print_weight_g /
                maximum_weight_g
            ) * 100

        else:

            material_usage_percent = 0


        # --------------------------------------
        # 19. RETURN FINAL RESPONSE
        # --------------------------------------

        return {

            "success": True,

            "status": "model_analyzed",

            "file_name": file.filename,


            "processing": {

                "unit": processing_unit,

                "unit_assumption": (
                    "STL coordinates are treated as millimeters"
                )

            },


            "material": material_key,

            "infill_percent": infill,


            "print_settings": {

                "layer_height_mm": LAYER_HEIGHT_MM,

                "wall_thickness_mm": WALL_THICKNESS_MM,

                "top_bottom_thickness_mm": TOP_BOTTOM_THICKNESS_MM,

                "correction_factor": PRINT_CORRECTION_FACTOR

            },


            "dimensions": {

                "x_mm": round(x_mm, 2),

                "y_mm": round(y_mm, 2),

                "z_mm": round(z_mm, 2),

                "max_dimension_mm": round(
                    max_dimension_mm,
                    2
                )

            },


            "volume": {

                "mm3": round(
                    volume_mm3,
                    2
                ),

                "cm3": round(
                    volume_cm3,
                    4
                )

            },


            "surface_area": {

                "mm2": round(
                    surface_area_mm2,
                    2
                )

            },


            "density_g_cm3": density,


            "solid_weight_g": round(
                solid_weight_g,
                2
            ),


            "calculation": {

                "shell_volume_mm3": round(
                    shell_volume_mm3,
                    2
                ),

                "internal_volume_mm3": round(
                    internal_volume_mm3,
                    2
                ),

                "infill_volume_mm3": round(
                    infill_volume_mm3,
                    2
                ),

                "estimated_material_volume_cm3": round(
                    estimated_material_volume_cm3,
                    4
                ),

                "material_usage_percent": round(
                    material_usage_percent,
                    2
                )

            },


            "estimated_print_weight_g": round(
                estimated_print_weight_g,
                2
            ),


            "note": (
                "STL files normally do not store unit information. "
                "This API processes standard 3D printing STL coordinates "
                "as millimeters. Print weight is estimated from model "
                "volume, surface geometry, wall thickness and infill. "
                "Actual slicer weight may vary depending on nozzle size, "
                "wall count, top/bottom layers, infill pattern, supports "
                "and other Cura settings."
            )

        }


    # ------------------------------------------
    # HANDLE KNOWN HTTP ERRORS
    # ------------------------------------------

    except HTTPException:

        raise


    # ------------------------------------------
    # HANDLE OTHER ERRORS
    # ------------------------------------------

    except Exception as e:

        raise HTTPException(
            status_code=500,
            detail=(
                "Could not process STL file: "
                f"{str(e)}"
            )
        )


    # ------------------------------------------
    # DELETE TEMPORARY FILE
    # ------------------------------------------

    finally:

        if (
            temp_path
            and os.path.exists(temp_path)
        ):

            os.remove(temp_path)
