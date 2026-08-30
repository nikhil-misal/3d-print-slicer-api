from fastapi import FastAPI, UploadFile, File, HTTPException, Form
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os
import math


app = FastAPI(
    title="Universal 3D Print Analyzer API",
    version="2.0.0"
)


# ==========================================
# CORS
# ==========================================

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)


# ==========================================
# MATERIAL DENSITIES
# Unit: g/cm³
# ==========================================

MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.25,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


# ==========================================
# UNIT CONVERSION
# How many millimeters in 1 STL unit
# ==========================================

UNIT_TO_MM = {
    "mm": 1.0,
    "cm": 10.0,
    "m": 1000.0,
    "inch": 25.4,
}


# ==========================================
# HOME
# ==========================================

@app.get("/")
def home():
    return {
        "status": "online",
        "message": "Universal 3D Print Analyzer API is running",
        "supported_units": list(UNIT_TO_MM.keys()),
        "supported_materials": list(MATERIAL_DENSITIES.keys())
    }


# ==========================================
# HELPER: CALCULATE PRINT WEIGHT
# ==========================================

def calculate_print_weight(
    volume_cm3: float,
    density: float,
    infill: float
):
    # Completely solid model weight
    solid_weight_g = volume_cm3 * density

    # Approximate material usage
    # Base shell/walls + infill contribution
    shell_factor = 0.25
    infill_factor = (infill / 100) * 0.70

    material_usage_factor = min(
        1.0,
        shell_factor + infill_factor
    )

    estimated_weight_g = (
        solid_weight_g *
        material_usage_factor
    )

    return (
        solid_weight_g,
        estimated_weight_g,
        material_usage_factor
    )


# ==========================================
# ANALYZE STL
# ==========================================

@app.post("/analyze")
async def analyze_stl(
    file: UploadFile = File(...),
    material: str = Form("PLA"),
    infill: float = Form(20),
    unit: str = Form("mm")
):

    # ------------------------------
    # FILE VALIDATION
    # ------------------------------

    if (
        not file.filename or
        not file.filename.lower().endswith(".stl")
    ):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )


    # ------------------------------
    # MATERIAL VALIDATION
    # ------------------------------

    material_key = material.upper().strip()

    if material_key not in MATERIAL_DENSITIES:
        raise HTTPException(
            status_code=400,
            detail=(
                "Unsupported material. "
                f"Supported: {list(MATERIAL_DENSITIES.keys())}"
            )
        )


    # ------------------------------
    # INFILL VALIDATION
    # ------------------------------

    if infill < 0 or infill > 100:
        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )


    # ------------------------------
    # UNIT VALIDATION
    # ------------------------------

    unit_key = unit.lower().strip()

    if unit_key not in UNIT_TO_MM:
        raise HTTPException(
            status_code=400,
            detail=(
                "Unsupported unit. "
                f"Supported: {list(UNIT_TO_MM.keys())}"
            )
        )


    temp_path = None


    try:

        # ==================================
        # SAVE FILE TEMPORARILY
        # ==================================

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


        # ==================================
        # LOAD STL
        # ==================================

        model = mesh.Mesh.from_file(
            temp_path
        )


        # ==================================
        # RAW VOLUME
        # STL UNIT³
        # ==================================

        raw_volume, _, _ = (
            model.get_mass_properties()
        )

        raw_volume = abs(
            float(raw_volume)
        )


        # ==================================
        # RAW DIMENSIONS
        # STL UNITS
        # ==================================

        min_values = (
            model.vectors.min(axis=(0, 1))
        )

        max_values = (
            model.vectors.max(axis=(0, 1))
        )

        raw_dimensions = (
            max_values - min_values
        )

        raw_x = float(raw_dimensions[0])
        raw_y = float(raw_dimensions[1])
        raw_z = float(raw_dimensions[2])


        # ==================================
        # CHECK MODEL VALIDITY
        # ==================================

        if (
            raw_volume <= 0 or
            not math.isfinite(raw_volume)
        ):
            raise HTTPException(
                status_code=400,
                detail=(
                    "Could not calculate a valid volume. "
                    "The STL may not be a closed/watertight model."
                )
            )


        # ==================================
        # CONVERT SELECTED UNIT TO MM
        # ==================================

        scale_to_mm = UNIT_TO_MM[
            unit_key
        ]


        # Dimensions
        length_mm = (
            raw_x * scale_to_mm
        )

        width_mm = (
            raw_y * scale_to_mm
        )

        height_mm = (
            raw_z * scale_to_mm
        )


        # ==================================
        # IMPORTANT:
        # VOLUME SCALE = SCALE³
        # ==================================

        volume_mm3 = (
            raw_volume *
            (scale_to_mm ** 3)
        )

        volume_cm3 = (
            volume_mm3 / 1000
        )


        # ==================================
        # MATERIAL WEIGHT
        # ==================================

        density = MATERIAL_DENSITIES[
            material_key
        ]

        (
            solid_weight_g,
            estimated_weight_g,
            material_usage_factor
        ) = calculate_print_weight(
            volume_cm3,
            density,
            infill
        )


        # ==================================
        # RESPONSE
        # ==================================

        return {

            "success": True,

            "status": "model_analyzed",

            "file_name": file.filename,

            "material": material_key,

            "infill_percent": infill,

            # --------------------------
            # UNIT INFORMATION
            # --------------------------

            "unit_used": unit_key,

            "scale_to_mm": scale_to_mm,


            # --------------------------
            # RAW STL DATA
            # --------------------------

            "raw_stl_dimensions": {
                "x": round(raw_x, 6),
                "y": round(raw_y, 6),
                "z": round(raw_z, 6)
            },

            "raw_stl_volume": round(
                raw_volume,
                8
            ),


            # --------------------------
            # FINAL DIMENSIONS
            # --------------------------

            "dimensions": {
                "x_mm": round(
                    length_mm,
                    2
                ),

                "y_mm": round(
                    width_mm,
                    2
                ),

                "z_mm": round(
                    height_mm,
                    2
                )
            },


            # --------------------------
            # FINAL VOLUME
            # --------------------------

            "volume": {
                "mm3": round(
                    volume_mm3,
                    3
                ),

                "cm3": round(
                    volume_cm3,
                    5
                )
            },


            # --------------------------
            # MATERIAL / WEIGHT
            # --------------------------

            "density_g_cm3": density,

            "solid_weight_g": round(
                solid_weight_g,
                3
            ),

            "material_usage_factor": round(
                material_usage_factor,
                4
            ),

            "estimated_print_weight_g": round(
                estimated_weight_g,
                3
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

        # ==================================
        # DELETE TEMP FILE
        # ==================================

        if (
            temp_path and
            os.path.exists(temp_path)
        ):
            os.remove(
                temp_path
            )
