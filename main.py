from fastapi import FastAPI, UploadFile, File, HTTPException, Query
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import tempfile
import os


app = FastAPI(
    title="3D Print Slicer API",
    version="1.1.0"
)


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
    "PLA+": 1.25,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}


# STL coordinate unit to millimeter conversion
UNIT_SCALE_TO_MM = {
    "mm": 1.0,
    "cm": 10.0,
    "m": 1000.0,
    "inch": 25.4,
}


@app.get("/")
def home():
    return {
        "status": "online",
        "message": "3D Print Slicer API is running",
        "version": "1.1.0"
    }


@app.post("/analyze")
async def analyze_stl(
    file: UploadFile = File(...),
    material: str = "PLA",
    infill: float = 20,
    unit: str = Query("mm")
):

    # Validate file
    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )

    # Validate material
    material_key = material.upper()

    if material_key not in MATERIAL_DENSITIES:
        raise HTTPException(
            status_code=400,
            detail=(
                "Unsupported material. Use: "
                f"{list(MATERIAL_DENSITIES.keys())}"
            )
        )

    # Validate infill
    if infill < 0 or infill > 100:
        raise HTTPException(
            status_code=400,
            detail="Infill must be between 0 and 100"
        )

    # Validate unit
    unit_key = unit.lower()

    if unit_key not in UNIT_SCALE_TO_MM:
        raise HTTPException(
            status_code=400,
            detail=(
                "Unsupported unit. Use: "
                "mm, cm, m, or inch"
            )
        )

    temp_path = None

    try:

        # Save uploaded STL temporarily
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

        # Unit scale
        scale_to_mm = UNIT_SCALE_TO_MM[unit_key]

        # RAW coordinates from STL
        raw_min = model.vectors.min(axis=(0, 1))
        raw_max = model.vectors.max(axis=(0, 1))

        raw_dimensions = raw_max - raw_min

        raw_x = float(raw_dimensions[0])
        raw_y = float(raw_dimensions[1])
        raw_z = float(raw_dimensions[2])

        # Calculate RAW STL volume
        raw_volume, _, _ = model.get_mass_properties()

        raw_volume = abs(float(raw_volume))

        # --------------------------------------------------
        # IMPORTANT UNIT CONVERSION
        #
        # Length: multiply by scale
        # Volume: multiply by scale³
        # --------------------------------------------------

        volume_mm3 = raw_volume * (
            scale_to_mm ** 3
        )

        volume_cm3 = volume_mm3 / 1000.0

        # Final dimensions in mm
        length_mm = raw_x * scale_to_mm
        width_mm = raw_y * scale_to_mm
        height_mm = raw_z * scale_to_mm

        # Material density
        density = MATERIAL_DENSITIES[material_key]

        # Solid model weight
        solid_weight_g = (
            volume_cm3 * density
        )

        # --------------------------------------------------
        # Estimated print material usage
        # --------------------------------------------------

        # Shell/base material contribution
        shell_factor = 0.25

        # Infill contribution
        infill_factor = (
            infill / 100.0
        ) * 0.70

        material_usage_factor = min(
            1.0,
            shell_factor + infill_factor
        )

        estimated_weight_g = (
            solid_weight_g *
            material_usage_factor
        )

        # Warning for suspiciously tiny models
        warning = None

        max_dimension = max(
            length_mm,
            width_mm,
            height_mm
        )

        if max_dimension < 1:
            warning = (
                "Model dimensions are smaller than 1 mm. "
                "The STL may use a different unit. "
                "Try unit=cm, m, or inch."
            )

        return {
            "success": True,
            "file_name": file.filename,

            "material": material_key,

            "infill_percent": infill,

            "unit_used": unit_key,

            "scale_to_mm": scale_to_mm,

            "raw_stl_dimensions": {
                "x": round(raw_x, 6),
                "y": round(raw_y, 6),
                "z": round(raw_z, 6)
            },

            "raw_stl_volume": round(
                raw_volume,
                8
            ),

            "volume": {
                "mm3": round(
                    volume_mm3,
                    4
                ),

                "cm3": round(
                    volume_cm3,
                    6
                )
            },

            "dimensions": {
                "x_mm": round(
                    length_mm,
                    4
                ),

                "y_mm": round(
                    width_mm,
                    4
                ),

                "z_mm": round(
                    height_mm,
                    4
                )
            },

            "density_g_cm3": density,

            "solid_weight_g": round(
                solid_weight_g,
                4
            ),

            "estimated_print_weight_g": round(
                estimated_weight_g,
                4
            ),

            "warning": warning,

            "status": "model_analyzed"
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

        # Delete temporary file
        if (
            temp_path and
            os.path.exists(temp_path)
        ):
            os.remove(temp_path)
