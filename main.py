from fastapi import FastAPI, UploadFile, File, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os
import numpy as np


app = FastAPI(
    title="3D Print Weight Calculator API",
    version="2.0.0"
)


# CORS
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


# Material densities in g/cm³
MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.24,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


# Standard print settings
LAYER_HEIGHT_MM = 0.20

# 0.4 mm nozzle with approximately 2 walls
WALL_THICKNESS_MM = 0.80

# Approximate top and bottom thickness
TOP_BOTTOM_THICKNESS_MM = 0.80

# Small correction for real slicer material behavior
PRINT_CORRECTION_FACTOR = 1.02


@app.get("/")
def home():
    return {
        "status": "online",
        "message": "3D Print Weight Calculator API is running"
    }


def calculate_surface_area(vectors):
    """
    Calculate STL triangle surface area in mm²
    """

    # Triangle vertices
    v0 = vectors[:, 0, :]
    v1 = vectors[:, 1, :]
    v2 = vectors[:, 2, :]

    # Two triangle edges
    edge1 = v1 - v0
    edge2 = v2 - v0

    # Cross product
    cross_product = np.cross(edge1, edge2)

    # Triangle area = 1/2 × magnitude of cross product
    triangle_areas = (
        np.linalg.norm(cross_product, axis=1) / 2
    )

    total_area = np.sum(triangle_areas)

    return abs(float(total_area))


@app.post("/analyze")
async def analyze_stl(
    file: UploadFile = File(...),
    material: str = "PLA",
    infill: float = 20
):

    # Validate file
    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )

    # Validate material
    material_key = material.upper().strip()

    if material_key not in MATERIAL_DENSITIES:
        raise HTTPException(
            status_code=400,
            detail=f"Unsupported material. Use: {list(MATERIAL_DENSITIES.keys())}"
        )

    # Validate infill
    if infill < 0 or infill > 100:
        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )

    temp_path = None

    try:

        # Save uploaded file temporarily
        with tempfile.NamedTemporaryFile(
            delete=False,
            suffix=".stl"
        ) as temp_file:

            temp_path = temp_file.name

            content = await file.read()

            if not content:
                raise HTTPException(
                    status_code=400,
                    detail="Uploaded STL file is empty"
                )

            temp_file.write(content)


        # Load STL
        model = mesh.Mesh.from_file(temp_path)


        # ==========================================
        # 1. CALCULATE RAW STL VOLUME
        # ==========================================

        volume_mm3, _, _ = model.get_mass_properties()

        volume_mm3 = abs(float(volume_mm3))

        if volume_mm3 <= 0:
            raise HTTPException(
                status_code=400,
                detail="Could not calculate a valid volume from this STL model"
            )


        volume_cm3 = volume_mm3 / 1000.0


        # ==========================================
        # 2. CALCULATE DIMENSIONS
        # STL coordinates are treated as millimeters
        # ==========================================

        min_values = model.vectors.min(axis=(0, 1))
        max_values = model.vectors.max(axis=(0, 1))

        dimensions_mm = max_values - min_values

        length = float(dimensions_mm[0])
        width = float(dimensions_mm[1])
        height = float(dimensions_mm[2])

        max_dimension_mm = max(
            length,
            width,
            height
        )


        # ==========================================
        # 3. CALCULATE SURFACE AREA
        # ==========================================

        surface_area_mm2 = calculate_surface_area(
            model.vectors
        )


        # ==========================================
        # 4. MATERIAL DENSITY
        # ==========================================

        density = MATERIAL_DENSITIES[
            material_key
        ]


        # ==========================================
        # 5. SOLID MODEL WEIGHT
        # ==========================================

        solid_weight_g = (
            volume_cm3 * density
        )


        # ==========================================
        # 6. ESTIMATE SHELL VOLUME
        #
        # Surface area × wall thickness
        # ==========================================

        shell_volume_mm3 = (
            surface_area_mm2 *
            WALL_THICKNESS_MM
        )


        # Shell volume can never exceed total model volume
        shell_volume_mm3 = min(
            shell_volume_mm3,
            volume_mm3 * 0.90
        )


        # ==========================================
        # 7. REMAINING INTERNAL VOLUME
        # ==========================================

        internal_volume_mm3 = max(
            0,
            volume_mm3 - shell_volume_mm3
        )


        # ==========================================
        # 8. ACTUAL INFILL MATERIAL
        # ==========================================

        infill_volume_mm3 = (
            internal_volume_mm3 *
            (infill / 100.0)
        )


        # ==========================================
        # 9. TOTAL PRINT MATERIAL VOLUME
        # ==========================================

        estimated_material_volume_mm3 = (
            shell_volume_mm3 +
            infill_volume_mm3
        )


        estimated_material_volume_cm3 = (
            estimated_material_volume_mm3 /
            1000.0
        )


        # ==========================================
        # 10. ESTIMATED PRINT WEIGHT
        # ==========================================

        estimated_print_weight_g = (
            estimated_material_volume_cm3 *
            density *
            PRINT_CORRECTION_FACTOR
        )


        # Prevent estimated weight from exceeding
        # corrected solid weight
        maximum_weight = (
            solid_weight_g *
            PRINT_CORRECTION_FACTOR
        )

        estimated_print_weight_g = min(
            estimated_print_weight_g,
            maximum_weight
        )


        # ==========================================
        # 11. MATERIAL USAGE PERCENTAGE
        # ==========================================

        material_usage_percent = (
            estimated_print_weight_g /
            maximum_weight
        ) * 100


        return {

            "success": True,

            "status": "model_analyzed",

            "file_name": file.filename,

            "processing_unit": "mm",

            "material": material_key,

            "infill_percent": infill,


            "print_settings": {

                "layer_height_mm": LAYER_HEIGHT_MM,

                "wall_thickness_mm": WALL_THICKNESS_MM,

                "top_bottom_thickness_mm": TOP_BOTTOM_THICKNESS_MM

            },


            "dimensions": {

                "x_mm": round(length, 2),

                "y_mm": round(width, 2),

                "z_mm": round(height, 2),

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
                "For standard 3D printing processing, STL coordinates "
                "are treated as millimeters. Weight is estimated using "
                "model geometry, surface area, wall thickness and infill."
            )

        }


    except HTTPException:
        raise


    except Exception as e:

        raise HTTPException(
            status_code=500,
            detail=(
                "Could not process STL file: "
                f"{str(e)}"
            )
        )


    finally:

        if (
            temp_path
            and os.path.exists(temp_path)
        ):
            os.remove(temp_path)
